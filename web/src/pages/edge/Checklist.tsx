import { type JSX, useState } from 'react'

import { useApp } from '../../api/store'
import type { Check, Report } from '../../api/types.gen'
import { Badge } from '../../components/Badge'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'

// The time before a token expires that pco warns of, as the engine and the
// doctor do (engine.ExpiryWarning).
export const expiryWarning = 30 * 24 * 3_600_000
const day = 24 * 3_600_000

// expiryNote says how soon a token expires once that is within the warning
// time, as pco credential list does, or nothing.
export function expiryNote(expiresOn: string | undefined, now: number): { tone: 'warn' | 'fail'; text: string } | undefined {
  if (!expiresOn) return undefined
  const left = Date.parse(expiresOn) - now
  if (!Number.isFinite(left) || left >= expiryWarning) return undefined
  if (left <= 0) return { tone: 'fail', text: 'expired' }
  if (left < day) return { tone: 'warn', text: 'expires in less than a day' }
  const days = Math.floor(left / day)
  return { tone: 'warn', text: days === 1 ? 'expires in 1 day' : `expires in ${days} days` }
}

// TokenLine is the status of a token, its expiry, and the warning when that
// is near.
export function TokenLine({ report }: { report: Report }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const [now] = useState(() => Date.now())
  const note = expiryNote(report.token.expiresOn, now)
  return (
    <>
      <Untrusted text={report.token.status || '-'} />
      {report.token.expiresOn && (
        <>
          , expires <Time at={report.token.expiresOn} nodeZone={nodeZone} />
        </>
      )}
      {note && (
        <>
          {' '}
          <Badge tone={note.tone}>{note.text}</Badge>
        </>
      )}
    </>
  )
}

// checkName is a check as pco credential check names it: the capability, on
// its account or zone.
export const checkName = (c: Check) => (c.scope ? `${c.capability} on ${c.scope}` : c.capability)

type Outcome = 'passed' | 'failed' | 'unanswered'

const outcomeOf = (c: Check): Outcome => (c.ok ? 'passed' : c.unanswered ? 'unanswered' : 'failed')

// The marks of pco credential check, and the words that go with them: a
// check Cloudflare did not answer says nothing of the token, so it never
// reads as a missing permission.
const marks: Readonly<Record<Outcome, [string, string]>> = {
  passed: ['✓', 'passed'],
  failed: ['✗', 'failed'],
  unanswered: ['?', 'not known, Cloudflare did not answer'],
}

// CheckLine is one check with its mark and word; by names the credential
// whose check it was, where checks of several are listed together.
export function CheckLine({ check, by }: { check: Check; by?: string }) {
  const outcome = outcomeOf(check)
  const [mark, word] = marks[outcome]
  return (
    <li className={`check check-${outcome}`}>
      <span className="check-mark" aria-hidden="true">
        {mark}
      </span>
      {by && (
        <span className="check-by">
          <Untrusted text={by} />:{' '}
        </span>
      )}
      <span className="check-name">
        <Untrusted text={checkName(check)} />
      </span>{' '}
      <span className="check-word">{word}</span>
      {!check.ok && check.detail && (
        <span className="check-detail">
          <Untrusted text={check.detail} />
        </span>
      )}
    </li>
  )
}

export interface CheckGroup {
  key: string
  title: string
  // a zone's group is in its account's
  zone: boolean
  checks: Check[]
}

// groupChecks puts the checks of a report under the token, each account and
// each zone of the account, in the order the report lists them; a check of a
// scope the report does not list is put under that scope at the end.
export function groupChecks(report: Report): CheckGroup[] {
  const left = new Set(report.checks)
  const take = (keep: (c: Check) => boolean) => {
    const got = report.checks.filter((c) => left.has(c) && keep(c))
    for (const c of got) left.delete(c)
    return got
  }
  const groups: CheckGroup[] = []
  const tokenChecks = take((c) => !c.scope && !c.scopeId)
  if (tokenChecks.length > 0) groups.push({ key: 'token', title: 'The token', zone: false, checks: tokenChecks })

  const zoneGroups = (accountId: string | undefined) =>
    report.zones
      .filter((z) => (accountId === undefined ? !report.accounts.some((a) => a.id === z.accountId) : z.accountId === accountId))
      .map((z) => ({ key: `zone:${z.id}`, title: `Zone ${z.name}`, zone: true, checks: take((c) => c.scopeId === z.id || (!c.scopeId && c.scope === z.name)) }))
      .filter((g) => g.checks.length > 0)

  for (const a of report.accounts) {
    const own = take((c) => c.scopeId === a.id || (!c.scopeId && c.scope === a.name))
    const zones = zoneGroups(a.id)
    if (own.length === 0 && zones.length === 0) continue
    groups.push({ key: `account:${a.id}`, title: `Account ${a.name || a.id}`, zone: false, checks: own }, ...zones)
  }
  groups.push(...zoneGroups(undefined))
  const scopes = [...new Set([...left].map((c) => c.scope || c.scopeId || ''))]
  for (const scope of scopes) {
    groups.push({ key: `scope:${scope}`, title: scope, zone: false, checks: take((c) => (c.scope || c.scopeId || '') === scope) })
  }
  return groups
}

// leftOutText names the zones a credential leaves out with the reason, as
// report.LeftOut does: "a.com, b.com left out: no DNS read".
export function leftOutText(report: Report | undefined): string {
  const excluded = report?.excluded ?? []
  if (excluded.length === 0) return ''
  const reasons = [...new Set(excluded.map((x) => x.reason))].join('; ')
  return `${excluded.map((x) => x.zone).join(', ')} left out: ${reasons}`
}

export function usableText(report: Report): string {
  if (report.usable) return 'yes'
  const failed = report.checks.filter((c) => !c.ok)
  if (failed.length > 0 && failed.every((c) => c.unanswered)) return 'not known, Cloudflare did not answer'
  return 'no'
}

// Checklist is the outcome of a check of a token in the words and marks of
// pco credential check: what the token sees, each check under its account
// and zone, the zones the credential leaves out with what would add them,
// whether the token can be used, and the probe records an earlier check
// left behind.
export function Checklist({ report }: { report: Report }): JSX.Element {
  const groups = groupChecks(report)
  return (
    <div className="checklist">
      <dl className="details">
        <dt>Token</dt>
        <dd>
          <TokenLine report={report} />
        </dd>
        <dt>Accounts</dt>
        <dd>{report.accounts.length === 0 ? 'none' : <Untrusted text={report.accounts.map((a) => a.name || a.id).join(', ')} />}</dd>
        <dt>Zones</dt>
        <dd>{report.zones.length === 0 ? 'none' : <Untrusted text={report.zones.map((z) => `${z.name} (${z.status})`).join(', ')} />}</dd>
      </dl>
      {groups.map((g) => (
        <section key={g.key} className={g.zone ? 'check-group check-zone' : 'check-group'} aria-label={g.title}>
          <h3 className="check-title">
            <Untrusted text={g.title} />
          </h3>
          {g.checks.length > 0 && (
            <ul className="checks">
              {g.checks.map((c, at) => (
                <CheckLine key={`${c.capability}:${c.scopeId ?? ''}:${at}`} check={c} />
              ))}
            </ul>
          )}
        </section>
      ))}
      {report.excluded.length > 0 && (
        <section className="check-group" aria-label="Zones left out">
          <h3 className="check-title">Zones left out</h3>
          <ul className="checks">
            {report.excluded.map((x) => (
              <li key={x.zoneId || x.zone} className="check check-left-out">
                <span className="check-mark" aria-hidden="true">
                  -
                </span>
                <span className="check-name">
                  <Untrusted text={`${x.zone} left out: ${x.reason}`} />
                </span>
                {x.detail && (
                  <span className="check-detail">
                    <Untrusted text={x.detail} />
                  </span>
                )}
              </li>
            ))}
          </ul>
        </section>
      )}
      <p className="check-usable">
        <b>Usable:</b> {usableText(report)}
      </p>
      {report.leftovers.length > 0 && (
        <p>
          Probe records left by an earlier check, to be removed by hand: <Untrusted text={report.leftovers.join(', ')} />
        </p>
      )}
    </div>
  )
}
