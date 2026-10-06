import { type FormEvent, useState } from 'react'

import { ApiError } from '../api/client'
import { explain } from '../api/errors'
import { useApp, useStore } from '../api/store'
import { About, notAffiliated } from '../app/About'
import { Button } from '../components/Button'
import { Field } from '../components/Field'
import { MarkIcon } from '../components/icons'
import { Untrusted } from '../components/Untrusted'

// The privileges pco asks for, and no more: a reader gets PVEAuditor, which
// holds Sys.Audit and the VM.Audit that says which guests it may see; an
// admin a role of its own with the two privileges pco checks on /.
const readerLines = 'pveum acl modify / --roles PVEAuditor --users <user>'
const adminLines = `pveum role add PCOAdmin --privs "Sys.Audit,Sys.Modify"
pveum acl modify / --roles PCOAdmin --users <user>`
const tokenLine = "pveum acl modify / --roles PCOAdmin --tokens '<user>!<name>'"

// The sentence for a refused sign-in: the web process's own words, but for
// what the page says better.
function refusal(e: ApiError): string {
  if (e.code === 'forbidden' && e.missing === 'Sys.Audit') return 'This user has no Sys.Audit on /: pco cannot show it anything.'
  if (e.code === 'invalid' && e.field === 'token') return 'This is not an API token of the form user@realm!tokenid=secret.'
  if (e.code === 'ticket_invalid') return e.message
  return explain(e).text
}

// SignInCard is the sign-in of the host profile: the session of
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
        {unauth?.ticket ? (
          <p>
            {signedOut
              ? 'You are signed out of pco. You are still signed in to Proxmox VE in this browser.'
              : 'This browser is signed in to Proxmox VE.'}
          </p>
        ) : (
          <p>
            Sign in to{' '}
            <a href={proxmox} target="_blank" rel="noopener noreferrer">
              Proxmox VE
            </a>{' '}
            in this browser at the same address, in another tab, then come back and sign in here with its session; this page keeps what it shows. pco must be opened
            under the same host name as Proxmox VE: the browser gives the session of Proxmox VE to that name only.
          </p>
        )}
        <Button variant="primary" disabled={busy !== undefined} onClick={() => void run('ticket', () => store.signInTicket())}>
          Sign in with the Proxmox VE session
        </Button>
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
        <p className="muted">The rights pco asks for, given on the node. For a reader:</p>
        <pre className="snippet">
          <code>{readerLines}</code>
        </pre>
        <p className="muted">For an admin, a role with Sys.Audit and Sys.Modify, nothing more:</p>
        <pre className="snippet">
          <code>{adminLines}</code>
        </pre>
        <p className="muted">
          A token with privilege separation needs the same line for itself, with --tokens in place of --users (and PVEAuditor for a reader&apos;s):
        </p>
        <pre className="snippet">
          <code>{tokenLine}</code>
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
