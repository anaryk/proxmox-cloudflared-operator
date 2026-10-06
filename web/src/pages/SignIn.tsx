import { type FormEvent, useState } from 'react'

import { ApiError } from '../api/client'
import { explain } from '../api/errors'
import { useApp, useStore } from '../api/store'
import { About, notAffiliated } from '../app/About'
import { Button } from '../components/Button'
import { Field } from '../components/Field'
import { MarkIcon } from '../components/icons'
import { Untrusted } from '../components/Untrusted'

// The two lines that make a token pco accepts: read-only with the role
// PVEAuditor (Sys.Audit on /), or an admin's with Administrator, which holds
// Sys.Modify.
const tokenLines = `pveum user token add <user@realm> pco --privsep 1
pveum acl modify / --tokens '<user@realm>!pco' --roles PVEAuditor`

// The sentence for a refused sign-in: the web process's own words, but for
// what the page says better.
function refusal(e: ApiError): string {
  if (e.code === 'forbidden' && e.missing === 'Sys.Audit') return 'This user has no Sys.Audit on /: pco cannot show it anything.'
  if (e.code === 'invalid' && e.field === 'token') return 'This is not an API token of the form user@realm!tokenid=secret.'
  if (e.code === 'ticket_invalid') return e.message
  return explain(e).text
}

// SignInCard is the sign-in of the host profile (spec-ui 4.1): the session of
// Proxmox VE in this browser, or a pasted API token. It is the page at
// /signin and the dialog that opens over a page whose session ended.
export function SignInCard({ onSignedIn }: { onSignedIn?: () => void }) {
  const store = useStore()
  const unauth = useApp((s) => s.unauthenticated)
  // why the page could not sign in by itself, as it loaded
  const authError = useApp((s) => s.authError)
  const [token, setToken] = useState('')
  const [shown, setShown] = useState(false)
  const [busy, setBusy] = useState<'ticket' | 'token'>()
  const [error, setError] = useState<{ method: 'ticket' | 'token'; text: string }>()
  const signedOut = store.signedOutHere()
  const host = window.location.hostname
  const proxmox = `https://${host.includes(':') ? `[${host}]` : host}:8006/`

  const run = async (method: 'ticket' | 'token', call: () => Promise<void>) => {
    setBusy(method)
    setError(undefined)
    try {
      await call()
      setToken('')
      onSignedIn?.()
    } catch (e) {
      setError({ method, text: e instanceof ApiError ? refusal(e) : String(e) })
    } finally {
      setBusy(undefined)
    }
  }

  const submit = (e: FormEvent) => {
    e.preventDefault()
    void run('token', () => store.signInToken(token.trim()))
  }

  return (
    <div className="signin">
      <section aria-labelledby="signin-ticket">
        <h2 id="signin-ticket">With your Proxmox VE session</h2>
        {signedOut && unauth?.ticket ? (
          <>
            <p>You are signed out of pco. You are still signed in to Proxmox VE in this browser.</p>
            <Button variant="primary" disabled={busy !== undefined} onClick={() => void run('ticket', () => store.signInTicket())}>
              Sign in with the Proxmox VE session
            </Button>
          </>
        ) : (
          <p>
            Sign in to <a href={proxmox}>Proxmox VE</a> in this browser at the same address, then reload this page. pco must be opened under the same host name as
            Proxmox VE: the browser gives the session of Proxmox VE to that name only.
          </p>
        )}
        {(error?.method === 'ticket' || (!error && authError)) && (
          <p className="form-error" role="alert">
            <Untrusted text={error?.text ?? (authError ? refusal(authError) : '')} />
          </p>
        )}
      </section>
      <form onSubmit={submit} aria-labelledby="signin-token">
        <h2 id="signin-token">Or with an API token</h2>
        <Field
          label="API token"
          hint={
            <>
              A token of Proxmox VE, <span className="mono">user@realm!tokenid=secret</span>.
            </>
          }
          error={error?.method === 'token' ? <Untrusted text={error.text} /> : undefined}
        >
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
        <Button type="submit" variant="primary" disabled={busy !== undefined || token.trim() === ''}>
          {busy === 'token' ? 'Signing in…' : 'Sign in'}
        </Button>
        <p className="muted">
          These two lines make a read-only token (Sys.Audit on /); give it the role Administrator instead of PVEAuditor for one that may change things
          (Sys.Modify on /):
        </p>
        <pre className="snippet">
          <code>{tokenLines}</code>
        </pre>
      </form>
    </div>
  )
}

// SignIn is the page at /signin.
export function SignIn({ onSignedIn }: { onSignedIn: () => void }) {
  const [about, setAbout] = useState(false)
  return (
    <main id="main" className="signin-page">
      <div className="card signin-card">
        <div className="card-head">
          <MarkIcon />
          <h1>Sign in to pco</h1>
        </div>
        <div className="card-body">
          <SignInCard onSignedIn={onSignedIn} />
        </div>
        <footer className="signin-foot muted">
          {notAffiliated}{' '}
          <button type="button" className="linkbtn" onClick={() => setAbout(true)}>
            About pco
          </button>
        </footer>
      </div>
      <About open={about} onClose={() => setAbout(false)} />
    </main>
  )
}
