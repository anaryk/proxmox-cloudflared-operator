import { useState } from 'react'

import { ApiError } from '../api/client'
import { useApp, useStore } from '../api/store'
import { About, notAffiliated } from '../app/About'
import { Button } from '../components/Button'
import { MarkIcon } from '../components/icons'
import { Untrusted } from '../components/Untrusted'
import { SignInAppliance } from './SignInAppliance'
import { refusal, TokenSignIn } from './SignInToken'

// HostSignIn is the sign-in of the host profile: the session of Proxmox VE
// in this browser, or a pasted API token.
function HostSignIn({ onSignedIn }: { onSignedIn?: () => void }) {
  const store = useStore()
  const unauth = useApp((s) => s.unauthenticated)
  // why the page could not sign in by itself, as it loaded
  const authError = useApp((s) => s.authError)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  // once the user tried a sign-in here, its own outcome is said instead
  const [tried, setTried] = useState(false)
  const signedOut = store.signedOutHere()
  const host = window.location.hostname
  const proxmox = `https://${host.includes(':') ? `[${host}]` : host}:8006/`

  const signInTicket = async () => {
    setTried(true)
    setBusy(true)
    setError(undefined)
    try {
      await store.signInTicket()
      onSignedIn?.()
    } catch (e) {
      setError(e instanceof ApiError ? refusal(e) : String(e))
    } finally {
      setBusy(false)
    }
  }

  const shown = error ?? (!tried && authError ? refusal(authError) : undefined)
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
        <Button variant="primary" disabled={busy} onClick={() => void signInTicket()}>
          Sign in with the Proxmox VE session
        </Button>
        {shown !== undefined && (
          <p className="form-error" role="alert">
            <Untrusted text={shown} />
          </p>
        )}
      </section>
      <TokenSignIn title="Or with an API token" onSignedIn={onSignedIn} onTry={() => setTried(true)} />
    </div>
  )
}

// SignInCard is the sign-in of the profile pco runs in: on a node the session
// of Proxmox VE or a token, in the appliance the user and password of Proxmox
// VE or a token. It is the page at /signin and the dialog that opens over a
// page whose session ended.
export function SignInCard({ onSignedIn }: { onSignedIn?: () => void }) {
  const appliance = useApp((s) => s.unauthenticated?.methods.includes('password') ?? s.session?.profile === 'appliance')
  return appliance ? <SignInAppliance onSignedIn={onSignedIn} /> : <HostSignIn onSignedIn={onSignedIn} />
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
