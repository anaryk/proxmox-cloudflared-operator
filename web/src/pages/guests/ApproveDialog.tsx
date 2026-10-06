import { useState } from 'react'

import { api, type ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { Approval, GuestListView, GuestView, State, UnapprovedGuest } from '../../api/types.gen'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { asApiError, Failure, Owner, ownerText, useAdminReason } from '../kit'

// What the dialog shows of a guest, and so what it sends: the identity, and
// for a guest that waits, the MACs and addresses the approval records.
export interface Asked {
  owner: string
  guest?: Pick<GuestView, 'name'>
  identity?: string
  waiting: boolean
  hostnames: string[]
  why: string[]
  macs: string[]
  addresses: string[]
}

const refOf = (g: Pick<UnapprovedGuest, 'kind' | 'vmid'>) => `${g.kind}/${g.vmid}`

// askedFor is what an approval of owner shows: what the state says it waits
// for, or else what the last listing showed of it.
export function askedFor(owner: string, st: Pick<State, 'unapproved'> | undefined, listed?: Pick<GuestListView, 'name' | 'identity'>): Asked {
  const waits = st?.unapproved.find((g) => refOf(g) === owner)
  if (waits) {
    return {
      owner,
      guest: { name: waits.name },
      identity: waits.identity,
      waiting: true,
      hostnames: waits.hostnames,
      why: waits.why,
      macs: waits.macs ?? [],
      addresses: waits.addresses ?? [],
    }
  }
  return { owner, guest: { name: listed?.name }, identity: listed?.identity, waiting: false, hostnames: [], why: [], macs: [], addresses: [] }
}

// The body of POST /api/v1/guests/approve: the identity shown, and the MACs
// and addresses shown, which the daemon checks against what it sees now.
export function approveBody(a: Asked): { owner: string; identity: string; macs?: string[]; addresses?: string[] } {
  return {
    owner: a.owner,
    identity: a.identity ?? '',
    ...(a.macs.length > 0 ? { macs: a.macs } : {}),
    ...(a.addresses.length > 0 ? { addresses: a.addresses } : {}),
  }
}

// The daemon's reason to refuse every approval while the last cycle did not
// list every guest (ApproveGuest).
export const incompleteReason = 'the last cycle did not list every guest'
export const noIdentityReason = 'Proxmox VE reports no identity for it, and an approval is of one'

// useApproveRefusal is why a guest cannot be approved now, or undefined.
export function useApproveRefusal(identity: string | undefined): string | undefined {
  const admin = useAdminReason()
  const complete = useApp((s) => s.state?.complete)
  if (admin) return admin
  if (!complete) return incompleteReason
  if (!identity) return noIdentityReason
  return undefined
}

function recordsText(a: Asked): string {
  const parts: string[] = []
  if (a.macs.length === 1) parts.push(`records MAC ${a.macs[0]}`)
  else if (a.macs.length > 1) parts.push(`records MACs ${a.macs.join(', ')}`)
  if (a.addresses.length === 1) parts.push(`allows address ${a.addresses[0]}`)
  else if (a.addresses.length > 1) parts.push(`allows addresses ${a.addresses.join(', ')}`)
  return parts.join(' and ')
}

// approvedText is what pco guest approve says after an approval.
export function approvedText(a: Approval, waited: boolean): string {
  const first = `Approved ${ownerText(a.owner, a.guest)} in identity ${a.identity}.`
  if (a.mode === 'approve' || waited || (a.addresses ?? []).length > 0) return `${first} From the next cycle its routes no longer wait for an approval.`
  return `${first} The admission mode is ${a.mode}: the approval matters only for its routes at observed until the mode is approve.`
}

// ApproveDialog repeats what an approval approves and sends exactly that. A
// guest that changed since is refused by the daemon; the dialog stays with
// its sentence and offers to look again.
export function ApproveDialog({ asked, onClose, onApproved, onLookAgain }: { asked?: Asked; onClose: () => void; onApproved: () => void; onLookAgain: () => void }) {
  const toast = useToast()
  const refusal = useApproveRefusal(asked?.identity)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const [shown, setShown] = useState(asked)
  if (shown !== asked) {
    setShown(asked)
    setError(undefined)
  }

  const close = () => {
    setError(undefined)
    onClose()
  }

  const approve = async () => {
    if (!asked) return
    setSending(true)
    setError(undefined)
    try {
      const done = await api<Approval>('POST', '/api/v1/guests/approve', approveBody(asked))
      toast(<Untrusted text={approvedText(done, asked.waiting)} />, 'ok')
      onApproved()
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  const records = asked ? recordsText(asked) : ''
  return (
    <Dialog
      open={asked !== undefined}
      onClose={close}
      title="Approve a guest"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="primary" disabled={sending} disabledReason={refusal} onClick={() => void approve()}>
            Approve
          </Button>
        </>
      }
    >
      {asked && (
        <>
          {asked.waiting ? (
            <p>
              <Owner owner={asked.owner} guest={asked.guest} /> waits for approval in identity{' '}
              <b className="mono">{asked.identity ? <Untrusted text={asked.identity} /> : '-'}</b>; approved, it publishes{' '}
              {asked.hostnames.length === 0
                ? '-'
                : asked.hostnames.map((h, at) => (
                    <span key={h}>
                      {at > 0 && ', '}
                      <Untrusted text={h} hostname />
                    </span>
                  ))}
              .
            </p>
          ) : (
            <p>
              <Owner owner={asked.owner} guest={asked.guest} /> has identity <b className="mono">{asked.identity ? <Untrusted text={asked.identity} /> : '-'}</b>{' '}
              in the last listing, and is approved in it: a guest re-created under the same VMID, or a clone, needs an approval of its own.
            </p>
          )}
          {asked.why.length > 0 && (
            <>
              <p>It waits because:</p>
              <ul className="plain-list">
                {asked.why.map((w) => (
                  <li key={w}>
                    <Untrusted text={w} />
                  </li>
                ))}
              </ul>
            </>
          )}
          {records && <p>Approving it {records}.</p>}
          <p className="muted">
            An approval admits a guest while the admission mode is approve, and releases what waits at observed, or on a soft-denied address, in either
            mode. The daemon refuses it when the guest changed since it was shown.
          </p>
          {sending && <Busy label="Approving" />}
          {error && (
            <Failure
              error={error}
              onLookAgain={() => {
                setError(undefined)
                onLookAgain()
              }}
              onTryAgain={() => void approve()}
            />
          )}
        </>
      )}
    </Dialog>
  )
}
