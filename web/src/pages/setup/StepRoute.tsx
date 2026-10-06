import { useState } from 'react'

import { useApp } from '../../api/store'
import type { ManualRouteView, RouteView, State } from '../../api/types.gen'
import { docsUrl } from '../../app/docs'
import { Link } from '../../app/Link'
import { Button } from '../../components/Button'
import { Field } from '../../components/Field'
import { CopyIcon } from '../../components/icons'
import { StateBadge } from '../../components/StateBadge'
import { Untrusted } from '../../components/Untrusted'
import { routeNote } from '../../text/words'
import { ManualRouteForm } from '../routes/ManualRouteForm'
import { normalHostname } from '../routes/manual'
import { manualPrefix, routeLink } from '../routes/routes'
import { blockErrors, type BlockValues, buildBlock, emptyBlock, hostOf } from './blockBuilder'
import type { StepProps } from './steps'

// The block is Notes text, not a command for a shell: it has its own copy
// button, which says whether the browser let the page copy.
function CopyBlock({ text }: { text: string }) {
  const [copied, setCopied] = useState<'copied' | 'failed'>()
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied('copied')
    } catch {
      setCopied('failed')
    }
  }
  return (
    <div className="command">
      <div className="command-line">
        <pre className="snippet" aria-label="The block for the Notes">
          <code>{text}</code>
        </pre>
        <Button small icon={<CopyIcon />} onClick={() => void copy()}>
          Copy
        </Button>
      </div>
      <p className="command-note">
        <span role="status">
          {copied === 'copied' && 'Copied.'}
          {copied === 'failed' && 'The browser did not let the page copy it: select the block and copy it yourself.'}
        </span>
      </p>
    </div>
  )
}

// The route of a hostname in the state, as it moves from no verified address
// to active, with the way to its diagnosis.
function Watched({ route, host }: { route?: RouteView; host: string }) {
  if (!route) {
    return (
      <p role="status" className="route-watch">
        Waiting for a route for <Untrusted text={host} hostname />: the next cycle of the daemon reads the Notes once the guest carries the tag.
      </p>
    )
  }
  const note = routeNote(route)
  return (
    <p role="status" className="route-watch">
      <Untrusted text={route.hostname} hostname /> <StateBadge state={route.state} />
      {note && (
        <>
          {' '}
          <Untrusted text={note} />
        </>
      )}{' '}
      <Link to={routeLink(route.hostname, route.owner)}>Diagnose it</Link>
    </p>
  )
}

function Builder({ st }: { st: State }) {
  const gateTag = useApp((s) => s.settings?.settings.gateTag)
  const version = useApp((s) => s.session?.version)
  const zones = st.zones.filter((z) => z.state === 'served').map((z) => z.name)
  const [label, setLabel] = useState('')
  const [zone, setZone] = useState('')
  const [v, setV] = useState<BlockValues>(emptyBlock)
  const set = <K extends keyof BlockValues>(k: K, value: BlockValues[K]) => setV((now) => ({ ...now, [k]: value }))

  // With zones to choose from the hostname is a name in front of one of them;
  // without, it is typed whole.
  const picked = zones.includes(zone) ? zone : (zones[0] ?? '')
  const hostname = zones.length > 0 ? hostOf(label, picked) : v.hostname
  const values: BlockValues = { ...v, hostname }
  // A field that was not filled in yet is not an error yet.
  const wrong = blockErrors(values)
  const said = (k: keyof BlockValues, filled: boolean) => (filled ? wrong[k] : undefined)
  const block = buildBlock(values)
  const https = v.scheme === 'https'
  const watched = block ? normalHostname(hostname) : undefined
  const route = watched ? st.routes.find((r) => r.hostname === watched) : undefined

  return (
    <div className="builder">
      <p>
        Give a guest the tag <span className="mono">{gateTag ? <Untrusted text={gateTag} /> : 'of the settings'}</span> in Proxmox VE and write its routes in its Notes. pco never
        writes tags or Notes itself. This builds the block for the Notes.
      </p>
      {zones.length > 0 ? (
        <div className="form-row">
          <Field label="Name" hint="The part in front of the zone, such as app." error={said('hostname', label.trim() !== '')}>
            {(c) => <input {...c} className="mono" value={label} autoComplete="off" spellCheck={false} onChange={(e) => setLabel(e.target.value)} />}
          </Field>
          <Field label="Zone">
            {(c) => (
              <select {...c} value={picked} onChange={(e) => setZone(e.target.value)}>
                {zones.map((z) => (
                  <option key={z} value={z}>
                    {z}
                  </option>
                ))}
              </select>
            )}
          </Field>
        </div>
      ) : (
        <Field label="Hostname" hint="The public name, such as app.example.com." error={said('hostname', v.hostname.trim() !== '')}>
          {(c) => <input {...c} className="mono" value={v.hostname} autoComplete="off" spellCheck={false} onChange={(e) => set('hostname', e.target.value)} />}
        </Field>
      )}
      <div className="form-row">
        <Field label="Scheme">
          {(c) => (
            <select {...c} value={v.scheme} onChange={(e) => set('scheme', e.target.value === 'https' ? 'https' : 'http')}>
              <option value="http">http</option>
              <option value="https">https</option>
            </select>
          )}
        </Field>
        <Field label="Port" hint="The port the service listens on, on the guest." error={said('port', v.port.trim() !== '')}>
          {(c) => <input {...c} className="mono" value={v.port} inputMode="numeric" autoComplete="off" spellCheck={false} onChange={(e) => set('port', e.target.value)} />}
        </Field>
      </div>
      <details className="builder-options">
        <summary>Options</summary>
        <Field label="Host header (optional)" hint="Sent to the service instead of the hostname." error={said('hostHeader', v.hostHeader.trim() !== '')}>
          {(c) => <input {...c} className="mono" value={v.hostHeader} autoComplete="off" spellCheck={false} onChange={(e) => set('hostHeader', e.target.value)} />}
        </Field>
        {https && (
          <>
            <Field label="SNI (optional)" hint="The name the certificate of the service is checked against." error={said('sni', v.sni.trim() !== '')}>
              {(c) => <input {...c} className="mono" value={v.sni} autoComplete="off" spellCheck={false} onChange={(e) => set('sni', e.target.value)} />}
            </Field>
            <label className="check">
              <input type="checkbox" checked={v.noTLSVerify} onChange={(e) => set('noTLSVerify', e.target.checked)} /> no-tls-verify: do not check the certificate of the service
            </label>
          </>
        )}
        <Field label="Via (optional)" hint="The network card of the guest, as net0, or an address of the guest to reach it by." error={said('via', v.via.trim() !== '')}>
          {(c) => <input {...c} className="mono" value={v.via} autoComplete="off" spellCheck={false} onChange={(e) => set('via', e.target.value)} />}
        </Field>
      </details>
      {block ? (
        <>
          <p>Paste this into the Notes of the guest, on its Summary page:</p>
          <CopyBlock key={block} text={block} />
          <Watched route={route} host={normalHostname(hostname)} />
        </>
      ) : (
        <p className="muted">The block appears here once the name and the port are filled in.</p>
      )}
      <p>
        <a href={docsUrl('annotations.md', version)}>How routes are written in the Notes</a>
      </p>
    </div>
  )
}

function Manual() {
  const [saved, setSaved] = useState<ManualRouteView>()
  const [round, setRound] = useState(0)
  return (
    <>
      <p>A hostname for an address that no guest names, within the allowed prefixes of the settings.</p>
      {saved && (
        <p role="status">
          Saved <Link to={routeLink(saved.hostname, `${manualPrefix}${saved.id}`)}>{`${manualPrefix}${saved.id}`}</Link>: the next cycle publishes it.{' '}
          <button
            type="button"
            className="linkbtn"
            onClick={() => {
              setSaved(undefined)
              setRound(round + 1)
            }}
          >
            Add another
          </button>
        </p>
      )}
      {!saved && (
        <details className="manual-details">
          <summary>The form of a manual route</summary>
          <ManualRouteForm key={round} onSaved={setSaved} onDeleted={() => undefined} onLookAgain={() => undefined} />
        </details>
      )}
    </>
  )
}

// StepRoute makes the first route in one of two ways: the block for the Notes
// of a guest, which the step then waits for in the state, or a manual route.
export function StepRoute({ st, p, go }: StepProps) {
  const first = [...st.routes].sort((a, b) => (a.hostname < b.hostname ? -1 : a.hostname > b.hostname ? 1 : 0)).slice(0, 5)
  return (
    <>
      {st.routes.length > 0 && (
        <div className="route-now">
          <p>
            The state holds {st.routes.length === 1 ? '1 route' : `${st.routes.length} routes`}
            {st.routes.length > first.length && `, the first ${first.length} here`}:
          </p>
          <ul className="plain-list">
            {first.map((r) => (
              <li key={`${r.hostname}\u0000${r.owner}`}>
                <Link to={routeLink(r.hostname, r.owner)}>
                  <Untrusted text={r.hostname} hostname />
                </Link>{' '}
                <StateBadge state={r.state} />
              </li>
            ))}
          </ul>
        </div>
      )}
      <div className="route-ways">
        <section aria-labelledby="route-guest">
          <h3 id="route-guest" className="wizard-sub">
            Annotate a guest
          </h3>
          <Builder st={st} />
        </section>
        <section aria-labelledby="route-manual">
          <h3 id="route-manual" className="wizard-sub">
            Or make a manual route
          </h3>
          <Manual />
        </section>
      </div>
      {p.routes > 0 && (
        <p className="wizard-next">
          <Button variant="primary" onClick={() => go('publish')}>
            Continue to publishing
          </Button>
        </p>
      )}
    </>
  )
}
