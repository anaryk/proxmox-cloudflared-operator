import { type FormEvent, useState } from 'react'

import { ApiError } from '../api/client'
import { useApp, useStore } from '../api/store'
import { Button } from '../components/Button'
import { Field } from '../components/Field'
import { Untrusted } from '../components/Untrusted'
import { refusal, TokenSignIn } from './SignInToken'

type Step = { name: 'password' } | { name: 'code'; kinds: string[] }

// codeHint says which codes the second factor of the account takes.
function codeHint(kinds: string[]): string {
  const totp = kinds.includes('totp')
  const recovery = kinds.includes('recovery')
  if (totp && recovery) return 'The code of your authenticator app, or one of your recovery codes.'
  if (recovery) return 'One of your recovery codes.'
  return 'The code of your authenticator app.'
}

// SignInAppliance is the sign-in of the appliance, which the browser holds
// no session of Proxmox VE for: the user, realm and password of Proxmox VE,
// then the code of its second factor when Proxmox VE asks for one; or a
// pasted API token. The password lives in the field until it is sent, and
// is cleared then whatever the answer.
export function SignInAppliance({ onSignedIn }: { onSignedIn?: () => void }) {
  const store = useStore()
  const realms = useApp((s) => s.unauthenticated?.realms) ?? []
  const [user, setUser] = useState('')
  const [chosen, setChosen] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [step, setStep] = useState<Step>({ name: 'password' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<{ field?: string; text: string }>()
  const realm = chosen || realms[0] || ''

  const fail = (e: unknown) => {
    if (!(e instanceof ApiError)) {
      setError({ text: String(e) })
      return
    }
    if (e.code === 'second_factor') {
      const given = (e.body as { kinds?: unknown }).kinds
      const kinds = Array.isArray(given) ? given.filter((k): k is string => typeof k === 'string') : []
      // A wrong code keeps the step; the first answer opens it.
      setError(step.name === 'code' ? { field: 'code', text: refusal(e) } : undefined)
      setStep({ name: 'code', kinds })
      return
    }
    if (e.code === 'ticket_invalid' && step.name === 'code') {
      // The step is over: three wrong codes, or two minutes.
      setStep({ name: 'password' })
      setCode('')
    }
    setError({ field: e.field, text: refusal(e) })
  }

  const signIn = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    const typed = password
    setPassword('')
    try {
      await store.signInPassword(user.trim(), realm, typed)
      onSignedIn?.()
    } catch (err) {
      fail(err)
    } finally {
      setBusy(false)
    }
  }

  const verify = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      await store.signInSecondFactor(code.trim())
      setCode('')
      onSignedIn?.()
    } catch (err) {
      setCode('')
      fail(err)
    } finally {
      setBusy(false)
    }
  }

  const startAgain = () => {
    setStep({ name: 'password' })
    setCode('')
    setError(undefined)
  }

  const errorOf = (field?: string) =>
    error !== undefined && (error.field === field || (field === undefined && !['user', 'realm', 'password', 'code'].includes(error.field ?? ''))) ? (
      <Untrusted text={error.text} />
    ) : undefined
  const formError = errorOf()

  return (
    <div className="signin">
      {step.name === 'password' ? (
        <form onSubmit={(e) => void signIn(e)} aria-labelledby="signin-password">
          <h2 id="signin-password">With your Proxmox VE user</h2>
          <Field label="User" error={errorOf('user')}>
            {(control) => (
              <input
                {...control}
                autoComplete="username"
                autoCapitalize="off"
                spellCheck={false}
                value={user}
                onChange={(e) => setUser(e.target.value)}
              />
            )}
          </Field>
          <Field
            label="Realm"
            hint={realms.length === 0 ? 'The realm of the user, such as pam or pve: the list of the node could not be read.' : undefined}
            error={errorOf('realm')}
          >
            {(control) =>
              realms.length > 0 ? (
                <select {...control} value={realm} onChange={(e) => setChosen(e.target.value)}>
                  {realms.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
              ) : (
                <input {...control} autoCapitalize="off" spellCheck={false} value={chosen} onChange={(e) => setChosen(e.target.value)} />
              )
            }
          </Field>
          <Field label="Password" error={errorOf('password')}>
            {(control) => (
              <input {...control} type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
            )}
          </Field>
          <Button type="submit" variant="primary" disabled={busy || user.trim() === '' || realm === '' || password === ''}>
            {busy ? 'Signing in…' : 'Sign in'}
          </Button>
          {formError !== undefined && (
            <p className="form-error" role="alert">
              {formError}
            </p>
          )}
          <p className="muted">
            pco hands the password to Proxmox VE on the node and keeps nothing of it. The second factor of the user, a TOTP or a recovery code, follows; a security
            key works only on Proxmox VE&apos;s own page.
          </p>
        </form>
      ) : (
        <form onSubmit={(e) => void verify(e)} aria-labelledby="signin-code">
          <h2 id="signin-code">The second factor of {user.trim()}</h2>
          <Field label="Code" hint={codeHint(step.kinds)} error={errorOf('code')}>
            {(control) => (
              <input
                {...control}
                autoComplete="one-time-code"
                inputMode={step.kinds.includes('recovery') ? 'text' : 'numeric'}
                spellCheck={false}
                value={code}
                onChange={(e) => setCode(e.target.value)}
              />
            )}
          </Field>
          <Button type="submit" variant="primary" disabled={busy || code.trim() === ''}>
            {busy ? 'Checking…' : 'Verify'}
          </Button>
          {formError !== undefined && (
            <p className="form-error" role="alert">
              {formError}
            </p>
          )}
          <button type="button" className="linkbtn" onClick={startAgain}>
            Start again with the password
          </button>
        </form>
      )}
      <TokenSignIn title="Or with an API token" onSignedIn={onSignedIn} />
    </div>
  )
}
