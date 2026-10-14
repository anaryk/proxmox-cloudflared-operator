import { useState } from 'react'

import { api, type ApiError } from '../../api/client'
import type { NodeView } from '../../api/types.gen'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { asApiError, Failure, useAdminReason } from '../kit'

export interface DrainAsk {
  node: NodeView
  // undrain ends the drain instead of starting one.
  undrain: boolean
}

// DrainDialog takes a node out of the shared tunnel's replicas, or puts it
// back. The last active replica is drained only with force, which stops
// the tunnel's only connector; a node that is not active yet keeps the
// intent and is drained once admitted. A drain Proxmox's maintenance mode
// made ends there, not here.
export function DrainDialog({ ask, onClose, onDone }: { ask?: DrainAsk; onClose: () => void; onDone: (n: NodeView) => void }) {
  const toast = useToast()
  const refusal = useAdminReason()
  const [force, setForce] = useState(false)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const [shown, setShown] = useState(ask)
  if (shown !== ask) {
    setShown(ask)
    setForce(false)
    setError(undefined)
  }

  const close = () => {
    setError(undefined)
    onClose()
  }

  const send = async () => {
    if (!ask) return
    setSending(true)
    setError(undefined)
    const name = encodeURIComponent(ask.node.name)
    try {
      const done = ask.undrain ? await api<NodeView>('POST', `/api/v1/nodes/${name}/undrain`, {}) : await api<NodeView>('POST', `/api/v1/nodes/${name}/drain`, { force })
      toast(
        ask.undrain ? (
          <>
            Node <Untrusted text={ask.node.name} /> is a replica of the shared tunnel again.
          </>
        ) : (
          <>
            Node <Untrusted text={ask.node.name} /> is drained: it leaves the shared tunnel&apos;s replicas; its pinned tunnel keeps the routes of its own guests.
          </>
        ),
        'ok',
      )
      onDone(done)
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  const maintenance = ask?.node.drained === 'maintenance'
  return (
    <Dialog
      open={ask !== undefined}
      onClose={close}
      title={ask?.undrain ? 'End the drain of a node' : 'Drain a node'}
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="primary" disabled={sending} disabledReason={refusal ?? (maintenance ? 'Proxmox has the node in maintenance mode; end it there' : undefined)} onClick={() => void send()}>
            {ask?.undrain ? 'End the drain' : 'Drain'}
          </Button>
        </>
      }
    >
      {ask && (
        <>
          {ask.undrain ? (
            <p>
              Node <b className="mono">{<Untrusted text={ask.node.name} />}</b> was drained
              {ask.node.drained && (
                <>
                  {' '}
                  (<Untrusted text={ask.node.drained} />)
                </>
              )}
              . Ending the drain makes it a replica of
              the shared tunnel again: it starts its connector and serves every shared route it reaches.
              {maintenance && ' Proxmox has it in maintenance mode, which drains it: end the maintenance there (ha-manager crm-command node-maintenance disable), and pco follows.'}
            </p>
          ) : (
            <>
              <p>
                Draining node <b className="mono">{<Untrusted text={ask.node.name} />}</b> takes it out of the shared tunnel&apos;s replicas: the routes it serves move to the other
                replicas, then its shared connector stops. The routes of its own guests stay on its pinned tunnel.
                {ask.node.state !== 'active' && ` The node is ${ask.node.state}: the drain applies once it is admitted.`}
              </p>
              <label className="check">
                <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} /> Drain it even as the last active replica, which stops the shared tunnel&apos;s only connector
              </label>
            </>
          )}
          {sending && <Busy label={ask.undrain ? 'Ending the drain' : 'Draining'} />}
          {error && <Failure error={error} onTryAgain={() => void send()} />}
        </>
      )}
    </Dialog>
  )
}
