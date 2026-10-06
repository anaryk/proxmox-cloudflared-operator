import { useState } from 'react'

import { api, ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { Conflict } from '../../api/types.gen'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { ErrorText } from './parts'

const sameName = (a: string, b: string) => a.replace(/\.$/, '').toLowerCase() === b.replace(/\.$/, '').toLowerCase()

// inTheWay is what stands in the way of a name according to the state: a
// record of someone else, or a record of this install that lost its marker.
export function inTheWay(st: { conflicts: readonly Conflict[]; lost: readonly string[] } | undefined, name: string): { conflict?: Conflict; lost: boolean } {
  const conflict = st?.conflicts.find((c) => sameName(c.name, name))
  return { conflict, lost: !conflict && (st?.lost ?? []).some((l) => sameName(l, name)) }
}

// AdoptDialog asks before pco replaces the record in the way of a hostname,
// or takes back one of its own that lost the marker, as pco adopt asks.
export function AdoptDialog({ name, onClose }: { name: string; onClose(): void }) {
  const st = useApp((s) => s.state)
  const toast = useToast()
  const [error, setError] = useState<unknown>()
  const [sending, setSending] = useState(false)
  const { conflict, lost } = inTheWay(st, name)
  const host = <Untrusted text={name} hostname />

  const send = async () => {
    setSending(true)
    setError(undefined)
    try {
      await api('POST', '/api/v1/adopt', { name })
      toast(<>Adoption of {host} requested; it is made by the next run that can.</>, 'ok')
      onClose()
    } catch (e) {
      setError(e)
    } finally {
      setSending(false)
    }
  }
  const refusedNow = !conflict && !lost ? 'the state shows nothing in the way of this name now' : sending ? 'it is being sent' : undefined
  return (
    <Dialog
      open
      onClose={onClose}
      title={<>{lost ? 'Take back' : 'Adopt'} {host}</>}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={() => void send()} disabledReason={refusedNow}>
            {lost ? 'Take it back' : 'Adopt it'}
          </Button>
        </>
      }
    >
      {conflict && (
        <p>
          {host} is held by a record of someone else in zone <Untrusted text={conflict.zone} />:{' '}
          <span className="mono">
            <Untrusted text={conflict.type} /> <Untrusted text={conflict.content} />
          </span>
          . Adopting replaces it with a record that points at the tunnel.
        </p>
      )}
      {lost && <p>{host} points at the tunnel of this install but lost its marker. Adopting takes the record back.</p>}
      {!conflict && !lost && <p>The state shows nothing in the way of {host} now: the daemon would refuse.</p>}
      <p>
        The record it replaces is kept in <span className="mono">/etc/pve/pco/adopted.jsonl</span> first. The change waits for a run in which the tunnel is
        verified and its connector is ready; the request is dropped after five minutes if no run could make it.
      </p>
      {error !== undefined && <ErrorText error={error} />}
      {error instanceof ApiError && error.code === 'refused' && <p className="muted">The dialog shows what the state says now.</p>}
    </Dialog>
  )
}
