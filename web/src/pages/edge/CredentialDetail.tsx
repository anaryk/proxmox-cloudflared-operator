import { type JSX, useState } from 'react'

import { api, type ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { CredentialView } from '../../api/types.gen'
import { navigate } from '../../app/router'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Field } from '../../components/Field'
import { Skeleton } from '../../components/Skeleton'
import { Time } from '../../components/Time'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { asApiError, Card, Failure, useAdminReason, useResource } from '../kit'
import { Checklist } from './Checklist'
import { CredentialState, depthText } from './Credentials'
import { DeepCheckDialog } from './DeepCheckDialog'

// RemoveDialog removes a credential once its label is typed. The daemon
// refuses one that still manages something, and says what; that is shown as
// it is.
export function RemoveDialog({ view, open, onClose, onRemoved }: { view: CredentialView; open: boolean; onClose: () => void; onRemoved: () => void }) {
  const toast = useToast()
  const [typed, setTyped] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()

  const close = () => {
    setTyped('')
    setError(undefined)
    onClose()
  }

  const remove = async () => {
    setSending(true)
    setError(undefined)
    try {
      await api('DELETE', `/api/v1/credentials/${encodeURIComponent(view.id)}`)
      toast(<Untrusted text={`Removed credential ${view.id}.`} />, 'ok')
      setTyped('')
      onRemoved()
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  const label = view.label || view.id
  return (
    <Dialog
      open={open}
      onClose={close}
      title="Remove a credential"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="danger" disabled={sending || typed !== label} onClick={() => void remove()}>
            Remove
          </Button>
        </>
      }
    >
      <p>
        pco stops using the token of <Untrusted text={label} />. The daemon refuses to remove a credential that still manages zones, tunnels or records,
        and says which.
      </p>
      <Field label={<>Type {<Untrusted text={label} />} to confirm</>}>
        {(control) => <input {...control} value={typed} autoComplete="off" spellCheck={false} onChange={(e) => setTyped(e.target.value)} />}
      </Field>
      {sending && <Busy label="Removing" />}
      {error && <Failure error={error} />}
    </Dialog>
  )
}

// CredentialDetail is /edge/credentials/:id: the last check of a token as a
// checklist, and the actions on it. A check only reads; a check of write
// access asks first, and a removal needs the label typed.
export function CredentialDetail({ id }: { id: string }): JSX.Element {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const refusal = useAdminReason()
  const list = useResource<CredentialView[]>('/api/v1/credentials')
  const [checked, setChecked] = useState<CredentialView>()
  const [running, setRunning] = useState<'shallow' | 'deep'>()
  const [error, setError] = useState<{ error: ApiError; deep: boolean }>()
  const [deepAsked, setDeepAsked] = useState(false)
  const [removing, setRemoving] = useState(false)

  if (list.error && !list.data) return <Failure error={list.error} onTryAgain={list.reload} />
  if (!list.data) return <Skeleton lines={4} label="Loading the credential" />
  const stored = list.data.find((v) => v.id === id)
  if (!stored) return <p className="muted">There is no credential <Untrusted text={id} />: it was removed, or never added.</p>
  const view = checked?.id === id ? checked : stored

  const check = async (deep: boolean) => {
    setDeepAsked(false)
    setRunning(deep ? 'deep' : 'shallow')
    setError(undefined)
    try {
      const got = await api<CredentialView>('POST', `/api/v1/credentials/${encodeURIComponent(id)}/check`, { deep })
      setChecked(got)
      list.reload()
    } catch (e) {
      setError({ error: asApiError(e), deep })
    } finally {
      setRunning(undefined)
    }
  }

  const busy = running !== undefined
  return (
    <div className="detail">
      <Card
        id="credential-summary"
        title={<Untrusted text={view.label || view.id} />}
        actions={
          <>
            <Button small disabled={busy} disabledReason={refusal} onClick={() => void check(false)}>
              Check
            </Button>
            <Button small disabled={busy} disabledReason={refusal} onClick={() => setDeepAsked(true)}>
              Check write access
            </Button>
            <Button small variant="danger" disabled={busy} disabledReason={refusal} onClick={() => setRemoving(true)}>
              Remove
            </Button>
          </>
        }
      >
        <dl className="details">
          <dt>Id</dt>
          <dd className="mono">
            <Untrusted text={view.id} />
          </dd>
          <dt>Kind</dt>
          <dd>{view.kind || '-'}</dd>
          <dt>State</dt>
          <dd>
            <CredentialState view={view} />
          </dd>
          <dt>Last check</dt>
          <dd>{view.report?.checkedAt ? <Time at={view.report.checkedAt} nodeZone={nodeZone} /> : 'never'}</dd>
          <dt>Depth</dt>
          <dd>{depthText(view)}</dd>
        </dl>
        {running === 'shallow' && <Busy label="Checking the token" />}
        {running === 'deep' && <Busy label="Checking write access" />}
        {error && <Failure error={error.error} onTryAgain={() => void check(error.deep)} />}
      </Card>
      <Card id="credential-checklist" title="The last check">
        {view.report ? (
          <>
            <Checklist report={view.report} />
            {!view.report.deep && <p>Write access was not tried.</p>}
          </>
        ) : (
          <p className="muted">This token has no check on record: run a check to see what it can do.</p>
        )}
      </Card>
      <DeepCheckDialog open={deepAsked} report={view.report} onClose={() => setDeepAsked(false)} onConfirm={() => void check(true)} />
      <RemoveDialog view={view} open={removing} onClose={() => setRemoving(false)} onRemoved={() => navigate('/edge/credentials')} />
    </div>
  )
}
