import { type FormEvent, useState } from 'react'

import { ApiError } from '../api/client'
import { explain } from '../api/errors'
import { useStore } from '../api/store'
import { Button } from '../components/Button'
import { Field } from '../components/Field'
import { Untrusted } from '../components/Untrusted'

// The privileges pco asks for, and no more: a reader gets PVEAuditor, which
// holds Sys.Audit and the VM.Audit that says which guests it may see; an
// admin a role of its own with the two privileges pco checks on /.
const readerLines = 'pveum acl modify / --roles PVEAuditor --users <user>'
const adminLines = `pveum role add PCOAdmin --privs "Sys.Audit,Sys.Modify"
pveum acl modify / --roles PCOAdmin --users <user>`
const tokenLine = "pveum acl modify / --roles PCOAdmin --tokens '<user>!<name>'"

// The refusals of a sign-in the web process words best itself: which
// address or account was refused, and for how long.
const ownWords = new Set(['ticket_invalid', 'forbidden', 'invalid', 'rate_limited', 'second_factor', 'second_factor_key'])

// refusal is the sentence for a refused sign-in: the web process's own
// words, but for what the page says better.
export function refusal(e: ApiError): string {
  if (e.code === 'forbidden' && e.missing === 'Sys.Audit') return 'This user has no Sys.Audit on /: pco cannot show it anything.'
  if (e.code === 'invalid' && e.field === 'token') return 'This is not an API token of the form user@realm!tokenid=secret.'
  if (ownWords.has(e.code)) return e.message.charAt(0).toUpperCase() + e.message.slice(1)
  return explain(e).text
}

// TokenSignIn is the sign-in with a pasted API token, in both profiles, with
// the lines that give a user or a token the rights pco asks for.
export function TokenSignIn({ title, onSignedIn, onTry }: { title: string; onSignedIn?: () => void; onTry?: () => void }) {
  const store = useStore()
  const [token, setToken] = useState('')
  const [shown, setShown] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    onTry?.()
    setBusy(true)
    setError(undefined)
    try {
      await store.signInToken(token.trim())
      setToken('')
      onSignedIn?.()
    } catch (err) {
      setError(err instanceof ApiError ? refusal(err) : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={(e) => void submit(e)} aria-labelledby="signin-token">
      <h2 id="signin-token">{title}</h2>
      <Field
        label="API token"
        hint={
          <>
            A token of Proxmox VE, <span className="mono">user@realm!tokenid=secret</span>.
          </>
        }
        error={error !== undefined ? <Untrusted text={error} /> : undefined}
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
      <Button type="submit" variant="primary" disabled={busy || token.trim() === ''}>
        {busy ? 'Signing in…' : 'Sign in'}
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
  )
}
