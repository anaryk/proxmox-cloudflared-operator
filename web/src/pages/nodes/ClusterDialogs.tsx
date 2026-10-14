import { useState } from 'react'

import { api, type ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { BlockView, HandoffResult, JoinTokenView, NodeView } from '../../api/types.gen'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Field } from '../../components/Field'
import { CopyIcon } from '../../components/icons'
import { Time } from '../../components/Time'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { marked } from '../../text/chars'
import { asApiError, Failure, useAdminReason } from '../kit'
import { nodeNameIssue } from './nodes'

// JoinTokenDialog makes a join token for an appliance agent to enrol with.
// The daemon keeps only the hash of the secret: the token is shown once,
// here, and is good for fifteen minutes and one use.
export function JoinTokenDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const refusal = useAdminReason()
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const [name, setName] = useState('')
  const [attempted, setAttempted] = useState(false)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const [token, setToken] = useState<JoinTokenView>()
  const [copied, setCopied] = useState<'copied' | 'failed'>()
  const issue = nodeNameIssue(name)

  const close = () => {
    setName('')
    setAttempted(false)
    setError(undefined)
    setToken(undefined)
    setCopied(undefined)
    onClose()
  }

  const make = async () => {
    setAttempted(true)
    if (issue) return
    setSending(true)
    setError(undefined)
    try {
      setToken(await api<JoinTokenView>('POST', '/api/v1/cluster/join-tokens', { node: name.trim() }))
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  const copy = async () => {
    if (!token) return
    try {
      await navigator.clipboard.writeText(token.token)
      setCopied('copied')
    } catch {
      setCopied('failed')
    }
  }

  return (
    <Dialog
      open={open}
      onClose={close}
      title="Make a join token"
      footer={
        token ? (
          <Button onClick={close}>Close</Button>
        ) : (
          <>
            <Button onClick={close}>Cancel</Button>
            <Button variant="primary" disabled={sending} disabledReason={refusal} onClick={() => void make()}>
              Make the token
            </Button>
          </>
        )
      }
    >
      {token ? (
        <>
          <p>
            The token for node <b className="mono">{<Untrusted text={token.node} />}</b>, shown once and kept nowhere else; it is good until{' '}
            <Time at={token.expires} nodeZone={nodeZone} /> and for one use.
          </p>
          <div className="token-box">
            <pre className="snippet">
              <code>
                <Untrusted text={token.token} />
              </code>
            </pre>
            <Button small icon={<CopyIcon />} onClick={() => void copy()}>
              Copy
            </Button>
          </div>
          <p className="command-note">
            <span role="status">
              {copied === 'copied' && 'Copied.'}
              {copied === 'failed' && 'The browser did not let the page copy it: select the token and copy it yourself.'}
            </span>
          </p>
          <p>
            On the node that hosts the container, hand it to <span className="mono">pco appliance install --kind agent --join -</span>, which reads it from its input. The
            agent then waits here for admission, with the fingerprint of its certificate.
          </p>
        </>
      ) : (
        <>
          <p>
            A join token enrols an appliance agent on another node of the cluster: the container joins with it, and waits here for admission. A host node needs none:{' '}
            <span className="mono">pco setup</span> on it joins the install.
          </p>
          <Field label="Node the agent runs on" hint="As Proxmox names it: lower-case letters, digits and hyphens between them." error={attempted ? issue : undefined}>
            {(control) => <input {...control} type="text" className="mono" autoComplete="off" spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} />}
          </Field>
          {sending && <Busy label="Making the token" />}
          {error && <Failure error={error} onTryAgain={() => void make()} />}
        </>
      )}
    </Dialog>
  )
}

// HandoffDialog has the controller release the lease for another node,
// which alone may take it for a while; the others hold back until then.
export function HandoffDialog({ open, nodes, controller, onClose }: { open: boolean; nodes: readonly NodeView[]; controller: string; onClose: () => void }) {
  const toast = useToast()
  const refusal = useAdminReason()
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const [to, setTo] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const candidates = nodes.filter((n) => n.name !== controller && n.state === 'active' && !n.drained)

  const close = () => {
    setTo('')
    setError(undefined)
    onClose()
  }

  const hand = async () => {
    setSending(true)
    setError(undefined)
    try {
      const res = await api<HandoffResult>('POST', '/api/v1/cluster/handoff', { to })
      toast(
        <>
          The lease goes to {res.to ? <Untrusted text={res.to} /> : 'the next node to acquire it'} after the running cycle; every other node holds back until{' '}
          <Time at={res.until} nodeZone={nodeZone} />.
        </>,
        'ok',
      )
      close()
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={close}
      title="Hand off the lease"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="primary" disabled={sending} disabledReason={refusal} onClick={() => void hand()}>
            Hand off
          </Button>
        </>
      }
    >
      <p>
        The controller, <b className="mono">{<Untrusted text={controller} />}</b>, releases the lease after its running cycle. The node chosen takes it within a few seconds and
        runs the next cycle; its first cycle writes nothing at Cloudflare until every node reported to it, or a minute passed. This page then sends you to the new controller.
      </p>
      <Field label="Hand it to" hint="An active node that is not drained; without one, the next node to ask for the lease takes it.">
        {(control) => (
          <select {...control} value={to} onChange={(e) => setTo(e.target.value)}>
            <option value="">The next node to acquire it</option>
            {candidates.map((n) => (
              // An option holds text only: the name is shown with its
              // controls of direction and invisible characters marked.
              <option key={n.name} value={n.name}>
                {marked(n.name)}
              </option>
            ))}
          </select>
        )}
      </Field>
      {sending && <Busy label="Handing off" />}
      {error && <Failure error={error} onTryAgain={() => void hand()} />}
    </Dialog>
  )
}

// UnblockDialog ends the block of an address every node blocks, after the
// admin made sure the MAC of its guest is on no other port.
export function UnblockDialog({ block, onClose }: { block?: BlockView; onClose: () => void }) {
  const toast = useToast()
  const refusal = useAdminReason()
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const [shown, setShown] = useState(block)
  if (shown !== block) {
    setShown(block)
    setError(undefined)
  }

  const close = () => {
    setError(undefined)
    onClose()
  }

  const unblock = async () => {
    if (!block) return
    setSending(true)
    setError(undefined)
    try {
      await api('POST', '/api/v1/cluster/unblock', { address: block.address })
      toast(
        <>
          <Untrusted text={block.address} /> is no longer blocked: every node serves it again from its next desired state.
        </>,
        'ok',
      )
      close()
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  return (
    <Dialog
      open={block !== undefined}
      onClose={close}
      title="End a block"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="danger" disabled={sending} disabledReason={refusal} onClick={() => void unblock()}>
            End the block
          </Button>
        </>
      }
    >
      {block && (
        <>
          <p>
            <b className="mono">{<Untrusted text={block.address} />}</b> is blocked on every node: the MAC of guest <Untrusted className="mono" text={block.guest} />
            {block.mac && (
              <>
                , <Untrusted className="mono" text={block.mac} />,
              </>
            )}{' '}
            was seen on port <Untrusted className="mono" text={block.port} /> of node <Untrusted className="mono" text={block.node} />. Whoever runs that port can answer for
            the guest&apos;s address, so no connector sends to it.
          </p>
          <p>
            A block ends by itself ten minutes after no agent reports the conflict and the hosting node attests the guest again. End it now only when you made sure the
            MAC is on no other port: a clone with the same MAC, stopped or re-addressed.
          </p>
          {sending && <Busy label="Ending the block" />}
          {error && <Failure error={error} onTryAgain={() => void unblock()} />}
        </>
      )}
    </Dialog>
  )
}
