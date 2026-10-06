import { type FormEvent, type JSX, useState } from 'react'

import { api, type ApiError } from '../../api/client'
import type { CredentialView } from '../../api/types.gen'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Field } from '../../components/Field'
import { asApiError, Failure, useAdminReason } from '../kit'
import { Checklist } from './Checklist'
import { DeepCheckDialog } from './DeepCheckDialog'

// The shape of a Cloudflare API token as the daemon checks it before it asks
// Cloudflare: 20 to 256 of these characters.
const tokenShape = /^[A-Za-z0-9_-]{20,256}$/
export const badTokenText = 'This is not a Cloudflare API token: it has 20 to 256 characters, all of A-Z a-z 0-9 _ -.'

// The credential view a refused token comes back with, when the daemon could
// check it: what the token can and cannot do.
function refusedView(e: ApiError): CredentialView | undefined {
  const v = e.credential as CredentialView | undefined
  return v && typeof v === 'object' && v.report ? v : undefined
}

// The outcome of an add or a check, as the step shows it.
type Outcome = { kind: 'added'; view: CredentialView } | { kind: 'checked'; view: CredentialView } | { kind: 'refused'; error: ApiError; view?: CredentialView }

// AddCredential adds a Cloudflare API token: a label and the token, checked
// by the daemon, which stores only a token it can use. The answer shows as
// the checklist of pco credential add; the token field is empty after any
// answer. A stored token was checked without writing, so the form then
// offers to check write access.
export function AddCredential({ onAdded }: { onAdded(view: CredentialView): void }): JSX.Element {
  const refusal = useAdminReason()
  const [label, setLabel] = useState('')
  const [token, setToken] = useState('')
  const [shown, setShown] = useState(false)
  const [invalid, setInvalid] = useState<{ label?: string; token?: string }>({})
  const [busy, setBusy] = useState<'adding' | 'checking'>()
  const [outcome, setOutcome] = useState<Outcome>()
  const [deepAsked, setDeepAsked] = useState(false)
  const [deepError, setDeepError] = useState<ApiError>()

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const wrong = {
      label: label.trim() === '' ? 'Give the credential a label.' : undefined,
      token: tokenShape.test(token.trim()) ? undefined : badTokenText,
    }
    setInvalid(wrong)
    if (wrong.label || wrong.token) return
    setBusy('adding')
    setOutcome(undefined)
    setDeepError(undefined)
    try {
      const view = await api<CredentialView>('POST', '/api/v1/credentials', { label: label.trim(), token: token.trim() })
      setOutcome({ kind: 'added', view })
      onAdded(view)
    } catch (err) {
      const error = asApiError(err)
      setOutcome({ kind: 'refused', error, view: refusedView(error) })
    } finally {
      // The token is gone from the page whatever the answer was.
      setToken('')
      setBusy(undefined)
    }
  }

  const deep = async (id: string) => {
    setDeepAsked(false)
    setBusy('checking')
    setDeepError(undefined)
    try {
      const view = await api<CredentialView>('POST', `/api/v1/credentials/${encodeURIComponent(id)}/check`, { deep: true })
      setOutcome({ kind: 'checked', view })
    } catch (err) {
      setDeepError(asApiError(err))
    } finally {
      setBusy(undefined)
    }
  }

  const stored = outcome?.kind === 'added' || outcome?.kind === 'checked' ? outcome.view : undefined
  const report = outcome?.view?.report
  return (
    <div className="add-credential">
      <form onSubmit={(e) => void submit(e)} aria-label="Add a Cloudflare API token" noValidate>
        <Field label="Label" hint="A name for the token, such as the account it is for." error={invalid.label}>
          {(control) => <input {...control} value={label} autoComplete="off" onChange={(e) => setLabel(e.target.value)} />}
        </Field>
        <Field label="Cloudflare API token" hint="It is sent to the daemon once and never shown again." error={invalid.token}>
          {(control) => (
            <span className="secret">
              <input
                {...control}
                type={shown ? 'text' : 'password'}
                autoComplete="off"
                spellCheck={false}
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
              <Button small aria-pressed={shown} onClick={() => setShown(!shown)}>
                {shown ? 'Hide' : 'Show'}
              </Button>
            </span>
          )}
        </Field>
        <Button type="submit" variant="primary" disabled={busy !== undefined} disabledReason={refusal}>
          Check and add
        </Button>
      </form>
      {busy === 'adding' && <Busy label="Checking the token with Cloudflare" />}
      {outcome?.kind === 'refused' && <Failure error={outcome.error} />}
      {outcome?.kind === 'added' && (
        <p role="status">
          Added credential {outcome.view.id} ({outcome.view.label}).
        </p>
      )}
      {report && <Checklist report={report} />}
      {stored && report && !report.deep && (
        <div className="deep-offer">
          <p>Write access was not tried.</p>
          <Button disabled={busy !== undefined} disabledReason={refusal} onClick={() => setDeepAsked(true)}>
            Check write access
          </Button>
        </div>
      )}
      {busy === 'checking' && <Busy label="Checking write access" />}
      {deepError && <Failure error={deepError} onTryAgain={() => stored && void deep(stored.id)} />}
      <DeepCheckDialog open={deepAsked} report={report} onClose={() => setDeepAsked(false)} onConfirm={() => stored && void deep(stored.id)} />
    </div>
  )
}
