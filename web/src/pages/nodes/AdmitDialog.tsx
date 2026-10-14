import { useState } from 'react'

import { api, type ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { NodeView } from '../../api/types.gen'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Field } from '../../components/Field'
import { Time } from '../../components/Time'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { asApiError, Failure, useAdminReason } from '../kit'
import { fingerprintIssue, fingerprintOf } from './nodes'

// The body of POST /api/v1/nodes/:name/admit: the fingerprint typed, as the
// daemon compares it with the certificate enrolled.
export const admitBody = (fingerprint: string) => ({ fingerprint: fingerprintOf(fingerprint), renew: false })

// JoinUses lists every use of the join token a node enrolled with: when,
// from where, and the container and MACs it said it was. An admin reads
// them before admitting an appliance agent: a second use, or one from an
// address that is not the container's, is the token in the wrong hands.
export function JoinUses({ uses, label }: { uses: NodeView['joinUses']; label: string }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  if (!uses || uses.length === 0) return null
  return (
    <ul className="join-uses" aria-label={label}>
      {uses.map((u, at) => (
        <li key={`${u.at}\u0000${u.source ?? ''}\u0000${at}`}>
          <Time at={u.at} nodeZone={nodeZone} /> from <span className="mono">{u.source ? <Untrusted text={u.source} /> : 'an unknown address'}</span>
          {u.vmid !== undefined && (
            <>
              , as container <span className="mono">{u.vmid}</span>
            </>
          )}
          {(u.macs ?? []).length > 0 && (
            <>
              , with MAC{(u.macs ?? []).length > 1 ? 's' : ''} <Untrusted className="mono" text={(u.macs ?? []).join(', ')} />
            </>
          )}
        </li>
      ))}
    </ul>
  )
}

// AdmitDialog admits a node that joins. An appliance agent is admitted
// with the fingerprint of its certificate, which the admin reads in the
// container with pco agent status and types here; the page checks its form
// and the daemon compares it. A host node, which issues its own
// certificates, needs none; one that stays joining because too many routes
// would move is let through, the routes moving first.
export function AdmitDialog({ node, onClose, onAdmitted }: { node?: NodeView; onClose: () => void; onAdmitted: (n: NodeView) => void }) {
  const toast = useToast()
  const refusal = useAdminReason()
  const [fingerprint, setFingerprint] = useState('')
  const [attempted, setAttempted] = useState(false)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const [shown, setShown] = useState(node)
  if (shown !== node) {
    setShown(node)
    setFingerprint('')
    setAttempted(false)
    setError(undefined)
  }
  const appliance = node?.kind === 'appliance'
  const issue = fingerprintIssue(fingerprint) ?? (appliance && fingerprint.trim() === '' ? 'an appliance agent is admitted with the fingerprint of its certificate' : undefined)

  const close = () => {
    setError(undefined)
    onClose()
  }

  const admit = async () => {
    if (!node) return
    setAttempted(true)
    if (issue) return
    setSending(true)
    setError(undefined)
    try {
      const admitted = await api<NodeView>('POST', `/api/v1/nodes/${encodeURIComponent(node.name)}/admit`, admitBody(fingerprint))
      toast(
        <>
          Node <Untrusted text={node.name} /> is admitted; every shared route it fails moves to its guest&apos;s node first.
        </>,
        'ok',
      )
      onAdmitted(admitted)
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  return (
    <Dialog
      open={node !== undefined}
      onClose={close}
      title="Admit a node"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="primary" disabled={sending} disabledReason={refusal} onClick={() => void admit()}>
            Admit
          </Button>
        </>
      }
    >
      {node && (
        <>
          <p>
            Node <b className="mono">{<Untrusted text={node.name} />}</b> is {node.state === 'joining' ? 'enrolled and waits for admission' : <Untrusted text={node.state} />}.
            {appliance ? (
              <> It is an appliance agent, in container {node.vmid ?? '-'}: its certificate is trusted once the fingerprint typed here is that of the certificate it enrolled.</>
            ) : (
              <> It is a host node, which issues its own certificates: no fingerprint is needed.</>
            )}
          </p>
          {(node.joinUses ?? []).length > 0 && (
            <>
              <p>Its join token was used:</p>
              <JoinUses uses={node.joinUses} label="Uses of the join token" />
              <p className="muted">One use, from the address of the container, is what a join looks like. Anything else is the token in the wrong hands: forget the node instead.</p>
            </>
          )}
          <Field
            label={appliance ? 'Fingerprint of the certificate' : 'Fingerprint of the certificate (optional)'}
            hint="The SHA-256 of the agent's certificate in hex, as pco agent status prints it in the container; at least 8 digits of it, colons allowed."
            error={attempted ? issue : undefined}
          >
            {(control) => <input {...control} type="text" className="mono" autoComplete="off" spellCheck={false} value={fingerprint} onChange={(e) => setFingerprint(e.target.value)} />}
          </Field>
          <p className="muted">Admission moves every shared route the node fails to its guest&apos;s node first; the node becomes active once those moves finished.</p>
          {sending && <Busy label="Admitting" />}
          {error && <Failure error={error} onTryAgain={() => void admit()} />}
        </>
      )}
    </Dialog>
  )
}
