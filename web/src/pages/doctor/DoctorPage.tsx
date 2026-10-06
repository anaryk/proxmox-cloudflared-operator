import './doctor.css'

import { useState } from 'react'

import { api, ApiError } from '../../api/client'
import { explain } from '../../api/errors'
import { useApp, useStore } from '../../api/store'
import type { DoctorCounts, Finding } from '../../api/types.gen'
import { Head } from '../../app/Head'
import { Badge } from '../../components/Badge'
import { Banner } from '../../components/Banner'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { CopyCommand } from '../../components/CopyCommand'
import { Empty } from '../../components/Empty'
import { LevelBadge } from '../../components/StateBadge'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { fixCommand } from '../../text/words'

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`

type Run =
  | { state: 'idle' }
  | { state: 'running'; since: number }
  | { state: 'findings'; at: string; findings: Finding[] }
  | { state: 'counts'; counts: DoctorCounts }
  | { state: 'failed'; error: ApiError }

const isCounts = (body: unknown): body is DoctorCounts =>
  typeof body === 'object' && body !== null && typeof (body as DoctorCounts).ok === 'number' && typeof (body as DoctorCounts).fail === 'number'

// The sentence a reader gets, and the admin's summary: how the checks ended
// and when they ran.
function Summary({ ok, warn, fail, at, nodeZone }: { ok: number; warn: number; fail: number; at: string; nodeZone?: string }) {
  return (
    <p className="doctor-summary" role="status">
      {ok} ok, {plural(warn, 'warning', 'warnings')}, {plural(fail, 'failure', 'failures')}, run at <Time at={at} nodeZone={nodeZone} />
    </p>
  )
}

// Fix is what to do about a finding: a command to copy when the fix is
// nothing but one, else the daemon's words, which may quote what a guest or
// Cloudflare wrote.
function Fix({ fix }: { fix: string }) {
  const words = fixCommand(fix)
  if (words.command) {
    return (
      <div className="finding-fix">
        <b>What to do</b>
        <CopyCommand cmd={words} root />
      </div>
    )
  }
  return (
    <p className="finding-fix">
      <b>What to do</b> <Untrusted text={fix} />
    </p>
  )
}

function FindingItem({ f }: { f: Finding }) {
  return (
    <li className="finding">
      <div className="finding-head">
        <LevelBadge level={f.level} />
        <b>
          <Untrusted text={f.check} />
        </b>
      </div>
      <p className="finding-detail">
        <Untrusted text={f.detail} />
      </p>
      {f.fix && <Fix fix={f.fix} />}
    </li>
  )
}

function Group({ id, title, tone, findings }: { id: string; title: string; tone: 'ok' | 'warn' | 'fail' | 'idle'; findings: Finding[] }) {
  if (findings.length === 0) return null
  return (
    <section className="card" aria-labelledby={id}>
      <div className="card-head">
        <h2 id={id}>{title}</h2>
        <Badge tone={tone}>{findings.length}</Badge>
      </div>
      <div className="card-body">
        <ul className="findings">
          {findings.map((f, at) => (
            <FindingItem key={`${f.check}:${at}`} f={f} />
          ))}
        </ul>
      </div>
    </section>
  )
}

const failures = (findings: readonly Finding[]) => findings.filter((f) => f.level === 'fail')

function Findings({ findings, at, nodeZone }: { findings: Finding[]; at: string; nodeZone?: string }) {
  const by = (level: string) => findings.filter((f) => f.level === level)
  const other = findings.filter((f) => f.level !== 'fail' && f.level !== 'warn' && f.level !== 'ok')
  if (findings.length === 0) {
    return <Empty title="The doctor reported no check">The daemon answered with no findings at all, which it never does. The journal on the node may say why.</Empty>
  }
  return (
    <>
      <Summary ok={by('ok').length} warn={by('warn').length} fail={failures(findings).length} at={at} nodeZone={nodeZone} />
      <Group id="doctor-fail" title="Failures" tone="fail" findings={by('fail')} />
      <Group id="doctor-warn" title="Warnings" tone="warn" findings={by('warn')} />
      <Group id="doctor-ok" title="Passed" tone="ok" findings={by('ok')} />
      <Group id="doctor-other" title="Other" tone="idle" findings={other} />
    </>
  )
}

function Intro({ admin, last, nodeZone }: { admin: boolean; last?: { fail: number; at: string }; nodeZone?: string }) {
  return (
    <section className="card" aria-labelledby="doctor-intro">
      <div className="card-head">
        <h2 id="doctor-intro">Not run yet</h2>
      </div>
      <div className="card-body">
        <p>The doctor probes this node and Cloudflare, so it runs only when you ask.</p>
        {admin ? (
          last && (
            <p className="muted">
              The last run in this browser had {plural(last.fail, 'failure', 'failures')}, at <Time at={last.at} nodeZone={nodeZone} />.
            </p>
          )
        ) : (
          <p>You get the counts of its checks. The findings are for admins: they name guests waiting for approval and hostnames of conflicts and lost markers that a reader may not see.</p>
        )}
      </div>
    </section>
  )
}

// DoctorPage runs the checks of pco doctor when asked, and only then: they
// probe the node and Cloudflare. An admin gets the findings, a reader the
// counts.
export function DoctorPage() {
  const store = useStore()
  const role = useApp((s) => s.session?.role)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const last = useApp((s) => s.doctorLast)
  const [run, setRun] = useState<Run>({ state: 'idle' })
  const admin = role === 'admin'

  const start = async () => {
    setRun({ state: 'running', since: Date.now() })
    try {
      const body = await api<Finding[] | DoctorCounts | undefined>('POST', '/api/v1/doctor', {})
      if (Array.isArray(body)) {
        const at = new Date().toISOString()
        setRun({ state: 'findings', at, findings: body })
        store.setDoctorLast({ fail: failures(body).length, at })
      } else if (isCounts(body)) {
        setRun({ state: 'counts', counts: body })
      } else {
        setRun({ state: 'failed', error: new ApiError(0, { code: 'internal', error: 'the doctor gave an answer the page does not know' }) })
      }
    } catch (e) {
      setRun({ state: 'failed', error: e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: String(e) }) })
    }
  }

  const ran = run.state === 'findings' || run.state === 'counts'
  return (
    <>
      <Head
        title="Doctor"
        description="The checks of pco doctor, run when you ask."
        actions={
          run.state === 'running' ? (
            <Busy label="Running the checks" since={run.since} />
          ) : (
            <Button variant="primary" onClick={() => void start()}>
              {ran ? 'Run again' : 'Run the checks'}
            </Button>
          )
        }
      />
      {run.state === 'failed' && (
        <div className="banners">
          <Banner tone="fail">
            The doctor did not run: <Untrusted text={explain(run.error).text} />
          </Banner>
        </div>
      )}
      {(run.state === 'idle' || run.state === 'failed') && <Intro admin={admin} last={last} nodeZone={nodeZone} />}
      {run.state === 'findings' && <Findings findings={run.findings} at={run.at} nodeZone={nodeZone} />}
      {run.state === 'counts' && (
        <section className="card" aria-labelledby="doctor-counts">
          <div className="card-head">
            <h2 id="doctor-counts">Result</h2>
          </div>
          <div className="card-body">
            <Summary {...run.counts} nodeZone={nodeZone} />
            <p>The findings are for admins: they name guests waiting for approval and hostnames of conflicts and lost markers that a reader may not see.</p>
          </div>
        </section>
      )}
    </>
  )
}
