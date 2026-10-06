import { useEffect, useState } from 'react'

import { api, ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { ClaimView, GuestView } from '../../api/types.gen'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { RefreshIcon } from '../../components/icons'
import { Skeleton } from '../../components/Skeleton'
import { Time } from '../../components/Time'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { ErrorText, errorMessage, Owner, readerReason, useAdmin } from './parts'
import { ownerName } from './routes'

// consequence says in one sentence what handing the hostname over does, as
// pco claims resolve says it.
export function consequence(claim: ClaimView, owner: string): string {
  const holder = ownerName(claim.holder, claim.guest)
  const to = ownerName(owner, claim.waiting.find((w) => w.owner === owner)?.guest)
  return `Resolving hands ${claim.hostname} to ${to}; ${holder} waits for it from then on, in the place in line its claim gives it.`
}

// HandDialog moves the claim on a hostname from its holder to one of the
// owners that wait for it, naming both.
function HandDialog({ claim, onClose, onMoved }: { claim: ClaimView; onClose: () => void; onMoved: () => void }) {
  const toast = useToast()
  const [owner, setOwner] = useState<string>()
  const [error, setError] = useState<unknown>()
  const [sending, setSending] = useState(false)
  const choices: { owner: string; guest?: GuestView; holder: boolean }[] = [
    { owner: claim.holder, guest: claim.guest, holder: true },
    ...claim.waiting.map((w) => ({ owner: w.owner, guest: w.guest, holder: false })),
  ]
  const send = async () => {
    if (!owner) return
    setSending(true)
    setError(undefined)
    try {
      await api('POST', '/api/v1/claims/resolve', { hostname: claim.hostname, owner })
      toast(
        <>
          The claim on <Untrusted text={claim.hostname} hostname /> is now held by <Untrusted text={owner} />.
        </>,
        'ok',
      )
      onMoved()
      onClose()
    } catch (e) {
      setError(e)
    } finally {
      setSending(false)
    }
  }
  const refused = error instanceof ApiError && error.code === 'refused'
  return (
    <Dialog
      open
      onClose={onClose}
      title={
        <>
          Hand <Untrusted text={claim.hostname} hostname /> to another owner
        </>
      }
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          {refused ? (
            <Button
              icon={<RefreshIcon />}
              onClick={() => {
                setError(undefined)
                setOwner(undefined)
                onMoved()
              }}
            >
              Look again
            </Button>
          ) : (
            <Button variant="primary" onClick={() => void send()} disabledReason={!owner || owner === claim.holder ? 'choose the owner to hand it to' : sending ? 'it is being sent' : undefined}>
              Hand it over
            </Button>
          )}
        </>
      }
    >
      <fieldset className="choices choices-column">
        <legend>Who holds the hostname</legend>
        {choices.map((c) => (
          <label key={c.owner} className="choice">
            <input type="radio" name="owner" value={c.owner} checked={(owner ?? claim.holder) === c.owner} onChange={() => setOwner(c.owner)} />
            <Owner owner={c.owner} guest={c.guest} /> {c.holder ? <span className="muted">holds it now</span> : <span className="muted">waits for it</span>}
          </label>
        ))}
      </fieldset>
      {owner && owner !== claim.holder && (
        <p>
          <Untrusted text={consequence(claim, owner)} />
        </p>
      )}
      {error !== undefined && <ErrorText error={error} />}
    </Dialog>
  )
}

// ClaimPanel shows who holds the hostname of a route and who waits for it,
// read when it opens: the claims are not part of the state.
export function ClaimPanel({ hostname }: { hostname: string }) {
  const admin = useAdmin()
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const [claims, setClaims] = useState<ClaimView[] | ApiError>()
  const [asked, setAsked] = useState(0)
  const [handing, setHanding] = useState(false)

  useEffect(() => {
    let on = true
    api<ClaimView[]>('GET', '/api/v1/claims', undefined, { background: true }).then(
      (c) => on && setClaims(c ?? []),
      (e: unknown) => on && setClaims(e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: errorMessage(e) })),
    )
    return () => {
      on = false
    }
  }, [asked])

  if (claims === undefined) return <Skeleton lines={3} label="Loading the claims" />
  if (claims instanceof ApiError) {
    return (
      <>
        <ErrorText error={claims} />
        <Button small icon={<RefreshIcon />} onClick={() => setAsked(asked + 1)}>
          Try again
        </Button>
      </>
    )
  }
  const claim = claims.find((c) => c.hostname === hostname)
  if (!claim) return <p className="muted">Nobody holds a claim on this hostname.</p>
  return (
    <div className="claim">
      <dl className="details">
        <dt>Holder</dt>
        <dd>
          <Owner owner={claim.holder} guest={claim.guest} />
        </dd>
        <dt>Since</dt>
        <dd>
          <Time at={claim.since} nodeZone={nodeZone} />
        </dd>
        <dt>State</dt>
        <dd>
          <Untrusted text={claim.state} />
        </dd>
        {claim.missingSince && (
          <>
            <dt>No longer asked for since</dt>
            <dd>
              <Time at={claim.missingSince} nodeZone={nodeZone} />
            </dd>
          </>
        )}
        <dt>Waiting</dt>
        <dd>
          {claim.waiting.length === 0 ? (
            <span className="muted">nobody</span>
          ) : (
            <ul className="plain-list">
              {claim.waiting.map((w) => (
                <li key={w.owner}>
                  <Owner owner={w.owner} guest={w.guest} /> <span className="muted">since</span> <Time at={w.since} nodeZone={nodeZone} />
                </li>
              ))}
            </ul>
          )}
        </dd>
      </dl>
      {claim.waiting.length > 0 && (
        <p>
          <Button onClick={() => setHanding(true)} disabledReason={admin ? undefined : readerReason}>
            Hand to …
          </Button>
        </p>
      )}
      {handing && <HandDialog claim={claim} onClose={() => setHanding(false)} onMoved={() => setAsked(asked + 1)} />}
    </div>
  )
}
