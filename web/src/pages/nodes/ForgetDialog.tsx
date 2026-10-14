import { useId, useState } from 'react'

import { api, type ApiError } from '../../api/client'
import type { ForgetResult, NodeView } from '../../api/types.gen'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { CopyCommand } from '../../components/CopyCommand'
import { Dialog } from '../../components/Dialog'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { delnodeCommand, forgetCompromisedCommand } from '../../text/words'
import { asApiError, Failure, useAdminReason } from '../kit'

// ForgetDialog takes a node out of the install: its entry is removed, its
// agent refused, its connections closed, its pinned tunnels deleted and
// the shared tunnel's secret rotated, so that the run token it holds stops
// working. Forgetting a node whose disk is out of the admin's hands rotates
// every secret of the install as well, which only root on the controller
// may ask for: the dialog gives that command and never sends it.
export function ForgetDialog({ node, rotateOnForget, onClose, onForgotten }: { node?: NodeView; rotateOnForget: boolean; onClose: () => void; onForgotten: (r: ForgetResult) => void }) {
  const toast = useToast()
  const refusal = useAdminReason()
  const id = useId()
  const [typed, setTyped] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const [result, setResult] = useState<ForgetResult>()
  const [shown, setShown] = useState(node)
  if (shown !== node) {
    setShown(node)
    setTyped('')
    setError(undefined)
    setResult(undefined)
  }

  const close = () => {
    setError(undefined)
    onClose()
  }

  const forget = async () => {
    if (!node) return
    setSending(true)
    setError(undefined)
    try {
      const done = await api<ForgetResult>('POST', `/api/v1/nodes/${encodeURIComponent(node.name)}/forget`, { compromised: false })
      toast(
        <>
          Node <Untrusted text={node.name} /> is forgotten.
        </>,
        'ok',
      )
      setResult(done)
      onForgotten(done)
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  const named = node !== undefined && typed.trim() === node.name
  return (
    <Dialog
      open={node !== undefined}
      onClose={close}
      title="Forget a node"
      footer={
        result ? (
          <Button onClick={close}>Close</Button>
        ) : (
          <>
            <Button onClick={close}>Cancel</Button>
            <Button variant="danger" disabled={sending} disabledReason={refusal ?? (named ? undefined : 'type the name of the node first')} onClick={() => void forget()}>
              Forget
            </Button>
          </>
        )
      }
    >
      {node && !result && (
        <>
          <p>
            Forgetting node <b className="mono">{<Untrusted text={node.name} />}</b> takes it out of the install: its entry is removed and its agent refused from its next request,
            its connections to the controller are closed, its pinned tunnels are deleted in the next cycle
            {rotateOnForget
              ? ', and the secret of the shared tunnel is rotated, so that the run token the node holds stops working; the other replicas restart with the new token one after the other.'
              : '. The secret of the shared tunnel is kept (cluster.rotateOnForget is off): the run token the node holds still works.'}
          </p>
          <p>
            A node that is still in the Proxmox cluster joins again with <span className="mono">pco setup</span> on it, or an appliance agent with a new join token.
          </p>
          <label htmlFor={id}>Type the name of the node to confirm</label>
          <input id={id} type="text" className="mono" autoComplete="off" spellCheck={false} value={typed} onChange={(e) => setTyped(e.target.value)} />
          <details>
            <summary>The disk of the node is out of your hands</summary>
            <p>
              Then every secret the node held is to be rotated: the Cloudflare tokens, the Proxmox token, every tunnel and the install&apos;s CA. Only root on the controller
              may ask for that, on the node itself, and only once the node is out of the Proxmox cluster, as a node still in it would receive every new secret:
            </p>
            <CopyCommand cmd={delnodeCommand(node.name)} root />
            <CopyCommand cmd={forgetCompromisedCommand(node.name)} root />
            <p className="muted">
              The disk also holds what pco cannot rotate: the corosync authkey, the SSH keys of root and the key of the cluster CA. The command lists them for you to decide on.
            </p>
          </details>
          {sending && <Busy label="Forgetting" />}
          {error && <Failure error={error} onTryAgain={() => void forget()} />}
        </>
      )}
      {result && (
        <>
          <p>
            Node <b className="mono">{<Untrusted text={result.node} />}</b> is forgotten.
          </p>
          <ul className="plain-list" aria-label="Done">
            {result.done.map((d) => (
              <li key={d}>
                <Untrusted text={d} />
              </li>
            ))}
          </ul>
          {(result.pending ?? []).length > 0 && (
            <>
              <p>Left for the cycles that follow, which a controller that takes over finishes too:</p>
              <ul className="plain-list" aria-label="Pending">
                {(result.pending ?? []).map((d) => (
                  <li key={d}>
                    <Untrusted text={d} />
                  </li>
                ))}
              </ul>
            </>
          )}
          {(result.notRotated ?? []).length > 0 && (
            <>
              <p>What the disk of the node holds that pco cannot rotate:</p>
              <ul className="plain-list" aria-label="Not rotated">
                {(result.notRotated ?? []).map((d) => (
                  <li key={d}>
                    <Untrusted text={d} />
                  </li>
                ))}
              </ul>
            </>
          )}
        </>
      )}
    </Dialog>
  )
}
