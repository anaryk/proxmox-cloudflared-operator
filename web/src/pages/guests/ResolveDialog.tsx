import { useState } from 'react'

import { api, type ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { ClaimView, State } from '../../api/types.gen'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { asApiError, Failure, Owner, ownerText } from '../kit'

// moveText says what handing a hostname to owner brings about, as pco claims
// resolve does: who holds it from then on, and when the new holder serves it
// as far as the state tells.
export function moveText(c: ClaimView, owner: string, st: Pick<State, 'unapproved' | 'routes' | 'mode' | 'hold'> | undefined): { sentence: string; outcome: string[] } {
  const guest = c.waiting.find((w) => w.owner === owner)?.guest
  const holder = ownerText(c.holder, c.guest)
  const sentence = `Resolving hands ${c.hostname} to ${ownerText(owner, guest)}; ${holder} waits for it from then on, in the place in line its claim gives it.`
  const outcome: string[] = []
  const waits = (st?.unapproved ?? []).some((g) => `${g.kind}/${g.vmid}` === owner && g.hostnames.includes(c.hostname))
  const routed = (st?.routes ?? []).some((r) => r.hostname === c.hostname && r.owner === owner)
  if (waits) outcome.push(`${owner} waits for approval: nobody serves ${c.hostname} until it is approved.`)
  else if (!routed) outcome.push(`${owner} names ${c.hostname} without a route for it: nobody serves it until ${owner} routes it.`)
  if (st?.mode === 'observe') outcome.push('The daemon only observes: the claim moves now, but nothing is published until publishing starts.')
  if (st?.hold) outcome.push(`The daemon holds (${st.hold}): the claim moves now, but ${owner} serves ${c.hostname} only once the daemon stops holding.`)
  if (outcome.length === 0) outcome.push(`From the next cycle ${owner} holds it, and serves it once its address is verified.`)
  return { sentence, outcome }
}

// ResolveDialog hands the claim on a hostname to one of those that claim
// it: a radio list of the holder and the claimants, and what that does.
export function ResolveDialog({ claim, onClose, onDone }: { claim?: ClaimView; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const st = useApp((s) => s.state)
  const [picked, setPicked] = useState(claim?.waiting[0]?.owner)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const [shown, setShown] = useState(claim)
  if (shown !== claim) {
    setShown(claim)
    setPicked(claim?.waiting[0]?.owner)
    setError(undefined)
  }

  const close = () => {
    setError(undefined)
    onClose()
  }

  const resolve = async () => {
    if (!claim || !picked) return
    setSending(true)
    setError(undefined)
    try {
      await api('POST', '/api/v1/claims/resolve', { hostname: claim.hostname, owner: picked })
      toast(<Untrusted text={`The claim on ${claim.hostname} is now held by ${picked}.`} />, 'ok')
      onDone()
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  const move = claim && picked && picked !== claim.holder ? moveText(claim, picked, st) : undefined
  return (
    <Dialog
      open={claim !== undefined}
      onClose={close}
      title="Hand a hostname to another owner"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="primary" disabled={sending || !move} onClick={() => void resolve()}>
            Hand it over
          </Button>
        </>
      }
    >
      {claim && (
        <>
          <p>
            <Untrusted text={claim.hostname} hostname /> is held by <Owner owner={claim.holder} guest={claim.guest} />.
          </p>
          <fieldset className="radio-list">
            <legend>Who holds it</legend>
            {[{ owner: claim.holder, guest: claim.guest, holder: true }, ...claim.waiting.map((w) => ({ ...w, holder: false }))].map((o) => (
              <label key={o.owner}>
                <input type="radio" name="resolve-owner" value={o.owner} checked={picked === o.owner} onChange={() => setPicked(o.owner)} />
                <Owner owner={o.owner} guest={o.guest} />
                {o.holder && <span className="muted"> (holds it now)</span>}
              </label>
            ))}
          </fieldset>
          {move && (
            <>
              <p>
                <Untrusted text={move.sentence} />
              </p>
              {move.outcome.map((line) => (
                <p key={line} className="muted">
                  <Untrusted text={line} />
                </p>
              ))}
            </>
          )}
          {sending && <Busy label="Handing it over" />}
          {error && (
            <Failure
              error={error}
              onLookAgain={() => {
                setError(undefined)
                onDone()
              }}
              onTryAgain={() => void resolve()}
            />
          )}
        </>
      )}
    </Dialog>
  )
}
