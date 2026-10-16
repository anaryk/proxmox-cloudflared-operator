import { useState } from 'react'

import { api } from '../../api/client'
import type { ApplyResult } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { Button } from '../../components/Button'
import { Untrusted } from '../../components/Untrusted'
import { applying, applyingAlways, counts, PlanSections } from '../routes/PlanPage'
import { ErrorText, readerReason, useAdmin } from '../routes/parts'
import { acknowledgeSegment, segmentKey, segmentName, waitingSegments } from '../networks/segments'
import type { Missing, StepId, StepProps } from './steps'

const whatIsMissing: Readonly<Record<Missing, { text: string; step: StepId; link: string }>> = {
  token: { text: 'a Cloudflare API token that can be used', step: 'token', link: 'Step 1' },
}

// StepPublish opens when a credential can be used. pco starts in
// observe-only mode: it plans, and changes nothing at Cloudflare until
// publishing is started here. The segments an appliance's card is on, and
// those a route waits for, are acknowledged with the same click, as the admin
// leaves them chosen: at observed nothing is served on a segment until then.
export function StepPublish({ st, p, go }: StepProps) {
  const admin = useAdmin()
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [said, setSaid] = useState<string>()
  const waiting = waitingSegments(st.segments)
  const [left, setLeft] = useState<ReadonlySet<string>>(new Set())
  const chosen = waiting.filter((s) => !left.has(segmentKey(s)))
  const toggle = (key: string) =>
    setLeft((was) => {
      const next = new Set(was)
      if (!next.delete(key)) next.add(key)
      return next
    })

  if (p.missing.length > 0) {
    return (
      <>
        <p>Publishing opens once pco has both of these, and still lacks:</p>
        <ul className="plain-list">
          {p.missing.map((m) => (
            <li key={m}>
              {whatIsMissing[m].text}:{' '}
              <button type="button" className="linkbtn" onClick={() => go(whatIsMissing[m].step)}>
                {whatIsMissing[m].link}
              </button>
            </li>
          ))}
        </ul>
      </>
    )
  }

  const send = async () => {
    setSending(true)
    setError(undefined)
    try {
      for (const s of chosen) await acknowledgeSegment(s)
      if (p.publishing) {
        setSaid('The segments are acknowledged; their routes at observed are served from the next cycle.')
        return
      }
      const res = await api<ApplyResult>('POST', '/api/v1/apply', {})
      setSaid(res?.leftObserveOnly ? applying : applyingAlways)
    } catch (e) {
      setError(e)
    } finally {
      setSending(false)
    }
  }

  const by = counts(st.actions)
  const follow = <Link to="/">Follow it on the Overview.</Link>
  let intro
  if (said) {
    intro = (
      <p role="status">
        {said} {follow}
      </p>
    )
  } else if (p.publishing) {
    intro = (
      <p role="status">
        Publishing has started: the daemon changes Cloudflare in every cycle. {follow}
      </p>
    )
  } else {
    intro = (
      <p>
        pco is observing only: it changes nothing at Cloudflare until you start publishing.{' '}
        {by.length === 0 ? (
          'The last cycle planned no action.'
        ) : (
          <>
            The last cycle planned{' '}
            {by.map(([kind, n], at) => (
              <span key={kind}>
                {at > 0 && ', '}
                <b className="num">{n}</b> <Untrusted text={kind} />
              </span>
            ))}
            .
          </>
        )}
      </p>
    )
  }
  return (
    <>
      {intro}
      <PlanSections compact />
      {waiting.length > 0 && !said && (
        <fieldset className="wizard-segments">
          <legend>Serve the routes at observed on these segments</legend>
          <p className="muted">Acknowledge a segment once you would trust any device on it with what its MAC can claim.</p>
          {waiting.map((s) => (
            <label key={segmentKey(s)} className="check">
              <input type="checkbox" checked={!left.has(segmentKey(s))} onChange={() => toggle(segmentKey(s))} /> <Untrusted text={segmentName(s)} />
              {s.own ? " (an appliance's card)" : ''}
              {s.routes > 0 ? `: ${s.routes} ${s.routes === 1 ? 'route waits' : 'routes wait'}` : ''}
            </label>
          ))}
        </fieldset>
      )}
      {(!p.publishing || chosen.length > 0) && !said && (
        <div className="wizard-next">
          <Button variant="primary" onClick={() => void send()} disabledReason={!admin ? readerReason : sending ? 'it is being sent' : undefined}>
            {p.publishing ? 'Acknowledge the segments' : 'Start publishing'}
          </Button>
        </div>
      )}
      {error !== undefined && <ErrorText error={error} />}
    </>
  )
}
