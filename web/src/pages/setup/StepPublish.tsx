import { useState } from 'react'

import { api } from '../../api/client'
import type { ApplyResult } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { Button } from '../../components/Button'
import { Untrusted } from '../../components/Untrusted'
import { applying, applyingAlways, counts, PlanSections } from '../routes/PlanPage'
import { ErrorText, readerReason, useAdmin } from '../routes/parts'
import type { Missing, StepId, StepProps } from './steps'

const whatIsMissing: Readonly<Record<Missing, { text: string; step: StepId; link: string }>> = {
  token: { text: 'a Cloudflare API token that can be used', step: 'token', link: 'Step 1' },
  route: { text: 'a route', step: 'route', link: 'Step 4' },
}

// StepPublish opens when a credential can be used and the state holds a route.
// pco starts in observe-only mode: it plans, and changes nothing at
// Cloudflare until publishing is started here.
export function StepPublish({ st, p, go }: StepProps) {
  const admin = useAdmin()
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [said, setSaid] = useState<string>()

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
      {!p.publishing && !said && (
        <div className="wizard-next">
          <Button variant="primary" onClick={() => void send()} disabledReason={!admin ? readerReason : sending ? 'it is being sent' : undefined}>
            Start publishing
          </Button>
        </div>
      )}
      {error !== undefined && <ErrorText error={error} />}
    </>
  )
}
