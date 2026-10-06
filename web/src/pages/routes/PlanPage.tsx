import { type JSX, type ReactNode, useEffect, useState } from 'react'

import { api } from '../../api/client'
import { useApp } from '../../api/store'
import type { Action, ApplyResult, State, Waiting } from '../../api/types.gen'
import { useLocation } from '../../app/router'
import { Badge } from '../../components/Badge'
import { Banner } from '../../components/Banner'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Skeleton } from '../../components/Skeleton'
import { type Column, Table } from '../../components/Table'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { budgetWait, unaffected } from '../../text/words'
import { AdoptDialog } from './AdoptDialog'
import { ConfirmDialog, UnaffectedList } from './ConfirmDialog'
import { ErrorText, PageHead, readerReason, RoutesNav, useAdmin } from './parts'
import { unset } from './routes'

// How many items of an entry of what waits are listed before "and N more",
// as pco plan lists them.
export const itemsShown = 20

const applying = 'Publishing has started: the daemon changes Cloudflare from the next cycle.'
const applyingAlways = 'Observe-only mode was off already: the daemon applies changes in every cycle.'

// waitingId is the fragment that names an entry of what waits, so that a
// link can point at it, /routes/plan#waiting-stale-zone-example.info, and the
// page focus it.
export function waitingId(w: Pick<Waiting, 'kind' | 'subject'>): string {
  const part = (s: string) => s.toLowerCase().replace(/[^a-z0-9.-]+/g, '-')
  return ['waiting', part(w.kind), part(w.subject)].filter(Boolean).join('-')
}

// budgetLine is the problem line of the Cloudflare budget stop, if the
// problems hold one.
export function budgetLine(problems: readonly string[]): string | undefined {
  return problems.find((p) => budgetWait(p).matched)
}

export const pendingOf = (actions: readonly Action[]) => actions.filter((a) => !a.applied)

function Items({ items }: { items: readonly string[] }) {
  const [all, setAll] = useState(false)
  if (items.length === 0) return null
  const shown = all ? items : items.slice(0, itemsShown)
  const more = items.length - shown.length
  return (
    <>
      <ul className="waiting-items mono">
        {shown.map((item) => (
          <li key={item}>
            <Untrusted text={item} />
          </li>
        ))}
      </ul>
      {more > 0 && (
        <button type="button" className="linkbtn" onClick={() => setAll(true)}>
          and {more} more
        </button>
      )}
    </>
  )
}

function Section({ id, title, compact, actions, children }: { id: string; title: string; compact?: boolean; actions?: ReactNode; children: ReactNode }) {
  const Heading = compact ? 'h3' : 'h2'
  return (
    <section className={compact ? 'plan-section plan-compact' : 'card plan-section'} aria-labelledby={id}>
      <div className={compact ? 'plan-head' : 'card-head'}>
        <Heading id={id}>{title}</Heading>
        {actions && <div className="plan-actions">{actions}</div>}
      </div>
      <div className={compact ? undefined : 'card-body'}>{children}</div>
    </section>
  )
}

function PendingTable({ actions }: { actions: readonly Action[] }) {
  const columns: Column<Action>[] = [
    {
      key: 'kind',
      header: 'Action',
      className: 'col-action',
      cell: (a) => (
        <>
          <Untrusted text={a.kind} />
          {a.destructive && (
            <>
              {' '}
              <Badge tone="fail">destructive</Badge>
            </>
          )}
        </>
      ),
    },
    { key: 'target', header: 'Target', lead: true, className: 'mono', cell: (a) => <Untrusted text={a.target} /> },
    { key: 'detail', header: 'Detail', cell: (a) => (a.detail ? <Untrusted text={a.detail} /> : <span className="muted">-</span>) },
    { key: 'held', header: 'Held', cell: (a) => (a.held ? <Untrusted text={a.held} /> : <span className="muted">-</span>) },
  ]
  return (
    <Table
      label="Pending actions"
      columns={columns}
      rows={actions}
      rowKey={(a) => `${a.kind}\u0000${a.accountId ?? ''}\u0000${a.target}`}
      height={320}
      rowClass={(a) => (a.destructive ? 'row-destructive' : undefined)}
    />
  )
}

// PlanSections is pco plan: the actions the last cycle did not carry out and
// why, what waits for a confirmation, the records of someone else in the way
// and the names that lost the marker of this install. The wizard shows it
// compact, in its last step.
export function PlanSections({ compact }: { compact?: boolean }): JSX.Element {
  const st = useApp((s) => s.state)
  const admin = useAdmin()
  const location = useLocation()
  const [confirming, setConfirming] = useState(false)
  const [adopting, setAdopting] = useState<string>()
  const hash = decodeURIComponent(new URL(location, 'https://page.invalid').hash.slice(1))

  useEffect(() => {
    if (hash) document.getElementById(hash)?.scrollIntoView?.({ block: 'center' })
  }, [hash, st])

  if (!st) return <Skeleton lines={5} label="Loading the plan" />
  if (unset(st.at)) return <p className="muted">Waiting for the first cycle: the plan is what a cycle found.</p>

  const pending = pendingOf(st.actions)
  const budget = budgetLine(st.problems)
  const nothing = pending.length === 0 && st.waiting.length === 0 && st.conflicts.length === 0 && st.lost.length === 0
  const adminOnly = admin ? undefined : readerReason
  return (
    <div className="plan">
      {budget && (
        <Banner tone="warn">
          <Untrusted text={budget} />: the pending actions wait for it, and the next cycles carry them out as the rate limit allows.
        </Banner>
      )}
      {nothing && <p>Nothing to do.</p>}
      {pending.length > 0 && (
        <Section id="plan-pending" title="Pending actions" compact={compact}>
          <PendingTable actions={pending} />
        </Section>
      )}
      {st.waiting.length > 0 && (
        <Section
          id="plan-waiting"
          title="Waits for a confirmation"
          compact={compact}
          actions={
            <Button variant="primary" small onClick={() => setConfirming(true)} disabledReason={adminOnly}>
              Review and confirm
            </Button>
          }
        >
          <ul className="waiting-list">
            {st.waiting.map((w) => {
              const id = waitingId(w)
              return (
                <li
                  key={`${w.kind}\u0000${w.subject}\u0000${w.detail}`}
                  id={id}
                  tabIndex={-1}
                  className={id === hash ? 'waiting-entry highlighted' : 'waiting-entry'}
                >
                  <Untrusted text={w.detail} />
                  <Items items={w.items ?? []} />
                </li>
              )
            })}
          </ul>
          {st.offer && (
            <p className="offer muted">
              Offer <span className="mono">{st.offer}</span>
            </p>
          )}
          <UnaffectedList actions={unaffected(st)} />
        </Section>
      )}
      {st.conflicts.length > 0 && (
        <Section id="plan-conflicts" title="Records of someone else that stand in the way" compact={compact}>
          <ul className="plain-list plan-names">
            {st.conflicts.map((c) => (
              <li key={`${c.zone}\u0000${c.name}`}>
                <span className="mono">
                  <Untrusted text={c.name} hostname />
                </span>{' '}
                <span className="muted">
                  in zone <Untrusted text={c.zone} />:
                </span>{' '}
                <span className="mono">
                  <Untrusted text={c.type} /> <Untrusted text={c.content} />
                </span>{' '}
                <Button small onClick={() => setAdopting(c.name)} disabledReason={adminOnly}>
                  Adopt
                </Button>
              </li>
            ))}
          </ul>
        </Section>
      )}
      {st.lost.length > 0 && (
        <Section id="plan-lost" title="Names that point at the tunnel but lost the marker of this install" compact={compact}>
          <ul className="plain-list plan-names">
            {st.lost.map((name) => (
              <li key={name}>
                <span className="mono">
                  <Untrusted text={name} hostname />
                </span>{' '}
                <Button small onClick={() => setAdopting(name)} disabledReason={adminOnly}>
                  Take back
                </Button>
              </li>
            ))}
          </ul>
        </Section>
      )}
      {confirming && <ConfirmDialog onClose={() => setConfirming(false)} />}
      {adopting !== undefined && <AdoptDialog name={adopting} onClose={() => setAdopting(undefined)} />}
    </div>
  )
}

// counts says how many actions of each kind are pending, for the question
// before publishing starts.
export function counts(actions: readonly Action[]): [string, number][] {
  const by = new Map<string, number>()
  for (const a of pendingOf(actions)) by.set(a.kind, (by.get(a.kind) ?? 0) + 1)
  return [...by.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
}

// StartPublishing leaves observe-only mode, as pco apply does, after a
// question that counts what the plan holds.
function StartPublishing({ st, onClose }: { st: State; onClose: () => void }) {
  const toast = useToast()
  const [error, setError] = useState<unknown>()
  const [sending, setSending] = useState(false)
  const by = counts(st.actions)
  const send = async () => {
    setSending(true)
    setError(undefined)
    try {
      const res = await api<ApplyResult>('POST', '/api/v1/apply', { confirmDeletes: false, offer: '' })
      toast(res?.leftObserveOnly ? applying : applyingAlways, 'ok')
      onClose()
    } catch (e) {
      setError(e)
    } finally {
      setSending(false)
    }
  }
  return (
    <Dialog
      open
      onClose={onClose}
      title="Start publishing"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={() => void send()} disabledReason={sending ? 'it is being sent' : undefined}>
            Start publishing
          </Button>
        </>
      }
    >
      <p>From the next cycle the daemon changes Cloudflare. The last cycle planned:</p>
      {by.length === 0 ? (
        <p className="muted">no action.</p>
      ) : (
        <ul className="plain-list">
          {by.map(([kind, n]) => (
            <li key={kind}>
              <b className="num">{n}</b> <Untrusted text={kind} />
            </li>
          ))}
        </ul>
      )}
      {st.waiting.length > 0 && <p>What waits for a confirmation still waits: starting to publish confirms nothing.</p>}
      {error !== undefined && <ErrorText error={error} />}
    </Dialog>
  )
}

// PlanPage is /routes/plan.
export function PlanPage() {
  const st = useApp((s) => s.state)
  const admin = useAdmin()
  const [starting, setStarting] = useState(false)
  const observing = st !== undefined && !unset(st.at) && st.mode === 'observe'
  return (
    <>
      <PageHead
        title="Plan"
        description="What the cycles would change at Cloudflare, and what waits for a confirmation."
        actions={
          observing && (
            <Button variant="primary" onClick={() => setStarting(true)} disabledReason={admin ? undefined : readerReason}>
              Start publishing
            </Button>
          )
        }
      />
      <RoutesNav current="plan" />
      <PlanSections />
      {starting && st && <StartPublishing st={st} onClose={() => setStarting(false)} />}
    </>
  )
}
