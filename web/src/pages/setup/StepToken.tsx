import { useState } from 'react'

import { api, type ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { CredentialView } from '../../api/types.gen'
import { docsUrl } from '../../app/docs'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Untrusted } from '../../components/Untrusted'
import { credentialState } from '../../text/words'
import { AddCredential } from '../edge/AddCredential'
import { Checklist } from '../edge/Checklist'
import { credentialPath, CredentialState } from '../edge/Credentials'
import { DeepCheckDialog } from '../edge/DeepCheckDialog'
import { asApiError, Failure, useAdminReason } from '../kit'
import { Link } from '../../app/Link'
import type { StepProps } from './steps'
import { accountTokenUrl, permissions, userTokenUrl } from './tokenTemplate'

// A token the daemon stores, as its last check found it, with what to do next:
// check again after granting more, or check that it can write.
function StoredToken({ view }: { view: CredentialView }) {
  const refusal = useAdminReason()
  const [checked, setChecked] = useState<CredentialView>()
  const [busy, setBusy] = useState<'shallow' | 'deep'>()
  const [error, setError] = useState<{ error: ApiError; deep: boolean }>()
  const [asking, setAsking] = useState(false)
  const shown = checked?.id === view.id ? checked : view
  const usable = credentialState(shown) === 'usable'

  const check = async (deep: boolean) => {
    setAsking(false)
    setBusy(deep ? 'deep' : 'shallow')
    setError(undefined)
    try {
      setChecked(await api<CredentialView>('POST', `/api/v1/credentials/${encodeURIComponent(view.id)}/check`, { deep }))
    } catch (e) {
      setError({ error: asApiError(e), deep })
    } finally {
      setBusy(undefined)
    }
  }

  return (
    <section className="stored-token" aria-label={`Credential ${shown.label || shown.id}`}>
      <h3>
        <Untrusted text={shown.label || shown.id} /> <CredentialState view={shown} />
      </h3>
      {shown.report ? (
        <details open={!usable}>
          <summary>What the check found</summary>
          <Checklist report={shown.report} />
        </details>
      ) : (
        <p>
          This token was never checked. <Link to={credentialPath(shown.id)}>Its page</Link> can check it.
        </p>
      )}
      {usable && shown.report && !shown.report.deep && (
        <div className="deep-offer">
          <p>Write access was not tried.</p>
          <Button disabled={busy !== undefined} disabledReason={refusal} onClick={() => setAsking(true)}>
            Check write access
          </Button>
        </div>
      )}
      {!usable && (
        <div className="deep-offer">
          {credentialState(shown) === 'unknown' ? (
            <p>Cloudflare did not answer, or the token was never checked: nothing says a permission is missing. Check again.</p>
          ) : (
            <p>Grant what the list asks for in Cloudflare, then check again: the token stays the same.</p>
          )}
          <Button disabled={busy !== undefined} disabledReason={refusal} onClick={() => void check(false)}>
            Check again
          </Button>
        </div>
      )}
      {busy === 'shallow' && <Busy label="Checking the token with Cloudflare" />}
      {busy === 'deep' && <Busy label="Checking write access" />}
      {error && <Failure error={error.error} onTryAgain={() => void check(error.deep)} />}
      <DeepCheckDialog open={asking} report={shown.report} onClose={() => setAsking(false)} onConfirm={() => void check(true)} />
    </section>
  )
}

// StepToken says what the token needs, opens Cloudflare's form with that
// chosen, and takes the token the admin made there.
export function StepToken({ st, p, go, onAdded }: StepProps & { onAdded: (v: CredentialView) => void }) {
  const node = useApp((s) => s.session?.node ?? '')
  const version = useApp((s) => s.session?.version)
  // The token added on this page keeps its own checklist, which would double
  // the stored one.
  const [added, setAdded] = useState<string>()
  const [more, setMore] = useState(false)
  const stored = st.credentials.filter((c) => c.id !== added)
  const form = more || added !== undefined || p.token !== 'usable'

  return (
    <>
      <p>
        pco talks to Cloudflare with an API token that you make in the Cloudflare dashboard. It needs three permissions and nothing else, and not the Global API Key:
      </p>
      <ul className="permissions">
        {permissions.map((x) => (
          <li key={x.row}>
            <b>{x.row}</b>, {x.why}
            {!x.link && ' (add this one yourself in the form)'}
          </li>
        ))}
      </ul>
      <p>
        Under Account Resources include the account that holds your zones, and under Zone Resources the zones you publish in. These buttons open Cloudflare&apos;s form in a new
        tab with Zone &gt; DNS &gt; Edit, Zone &gt; Zone &gt; Read and the name chosen; Account &gt; Cloudflare Tunnel &gt; Edit you add there yourself. Nothing is made until you
        press Create Token. The check after you paste the token names anything that is missing.
      </p>
      <div className="token-links">
        <a className="btn btn-primary" href={userTokenUrl(node)} target="_blank" rel="noopener noreferrer">
          Create a user token
        </a>
        <a className="btn" href={accountTokenUrl(node)} target="_blank" rel="noopener noreferrer">
          Create an account token
        </a>
      </div>
      <p className="muted">
        Both kinds work: a user token acts as you, an account token is a service credential of the account. If a form opens without a permission, add the row by hand.{' '}
        <a href={docsUrl('cloudflare-token.md', version)}>The token page</a> has the steps in the dashboard.
      </p>

      {stored.map((c) => (
        <StoredToken key={c.id} view={c} />
      ))}

      {form ? (
        <>
          <h3 className="wizard-sub">Give pco the token</h3>
          <AddCredential
            onAdded={(v) => {
              setAdded(v.id)
              setMore(false)
              onAdded(v)
            }}
          />
        </>
      ) : (
        <p>
          <Button onClick={() => setMore(true)}>Add another token</Button>
        </p>
      )}

      {(p.token === 'usable' || added !== undefined) && (
        <p className="wizard-next">
          <Button variant="primary" onClick={() => go('zones')}>
            Continue to the zones
          </Button>
        </p>
      )}
    </>
  )
}
