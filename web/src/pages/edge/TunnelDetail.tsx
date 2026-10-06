import type { JSX } from 'react'

import { useApp } from '../../api/store'
import type { ConnectorStatus, RolloutView, State, TunnelTraffic, TunnelView } from '../../api/types.gen'
import { EventsTable } from '../../app/EventsTable'
import { CopyCommand } from '../../components/CopyCommand'
import { RogueIcon } from '../../components/icons'
import { Skeleton } from '../../components/Skeleton'
import { Time } from '../../components/Time'
import { TrafficChart } from '../../components/TrafficChart'
import { Untrusted } from '../../components/Untrusted'
import { observeOnlyRefusal } from '../../gen/words.gen'
import { connectorText, rogueText, rotateCommand, verifiedText } from '../../text/words'
import { accountName, Card, credentialLabel } from '../kit'

// tunnelLabel tells the tunnels of an install apart, which all have the same
// name: by the name of the account and the first 8 characters of the id,
// "Main · 3f2a91c0".
export function tunnelLabel(st: Pick<State, 'credentials'> | undefined, t: Pick<TunnelView, 'accountId' | 'id'>): string {
  return `${accountName(st, t.accountId)} · ${t.id ? t.id.slice(0, 8) : '-'}`
}

export const tunnelPath = (account: string) => `/edge/tunnels/${encodeURIComponent(account)}`

// verifiedLine is the verified word of pco status, with why for a tunnel
// that was held or not checked.
export function verifiedLine(t: TunnelView, hold: string | undefined): string {
  const word = verifiedText(t)
  if (t.unchecked) return hold ? `${word}: ${hold}` : word
  if (t.held) return `${word}: ${t.held}`
  return word
}

export function rolloutText(r: RolloutView | undefined): string {
  if (!r) return '-'
  return `version ${r.version} runs on ${r.connectors === 1 ? '1 connector' : `${r.connectors} connectors`}`
}

export const connectorOf = (st: Pick<State, 'connectors'>, t: Pick<TunnelView, 'id'>): ConnectorStatus | undefined =>
  t.id ? st.connectors.find((c) => c.tunnelId === t.id) : undefined

// The connector of the tunnel in the words of pco status: none, or what
// connector.Status says with the flags of its token and its metrics port.
export const connectorLine = (c: ConnectorStatus | undefined) => (c ? connectorText(c) : 'none')

function Connector({ status, traffic }: { status?: ConnectorStatus; traffic?: TunnelTraffic }) {
  return (
    <dl className="details">
      <dt>Connector</dt>
      <dd>{connectorLine(status)}</dd>
      {status?.connectorId && (
        <>
          <dt>Connector id</dt>
          <dd className="mono">
            <Untrusted text={status.connectorId} />
          </dd>
        </>
      )}
      {traffic?.cloudflared && (
        <>
          <dt>cloudflared</dt>
          <dd>
            <Untrusted text={traffic.cloudflared} />
          </dd>
        </>
      )}
      {traffic && traffic.edges.length > 0 && (
        <>
          <dt>Edge locations</dt>
          <dd>
            <Untrusted text={traffic.edges.map((e) => e.location).join(', ')} />
          </dd>
        </>
      )}
      {traffic && traffic.rttMs.length > 0 && (
        <>
          <dt>Round trip</dt>
          <dd className="num">{traffic.rttMs.map((ms) => `${ms.toFixed(1)} ms`).join(', ')}</dd>
        </>
      )}
    </dl>
  )
}

// Rogues is the red block of a tunnel that Cloudflare lists connectors on
// that pco does not run, or whose token Cloudflare refuses: what is known of
// them, and the command that gives the tunnel a new secret, to run as root
// on the node. The page never runs it: the daemon takes it from root only.
export function Rogues({ st, t, observeOnly }: { st: State; t: TunnelView; observeOnly: boolean }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const rogues = st.rogueConnectors.filter((r) => r.accountId === t.accountId)
  const refused = connectorOf(st, t)?.tokenRefused === true
  if (rogues.length === 0 && !refused) return null
  return (
    <section className="rogue" aria-label="Connectors pco does not run">
      <h3 className="rogue-head">
        <RogueIcon />
        {rogues.length > 0
          ? rogues.length === 1
            ? '1 connector pco does not run'
            : `${rogues.length} connectors pco does not run`
          : 'Cloudflare refuses the token of its connector'}
      </h3>
      {rogues.length > 0 && (
        <>
          <ul className="plain-list">
            {rogues.map((r) => (
              <li key={r.id}>
                <Untrusted text={rogueText(r)} />, first seen <Time at={r.since} nodeZone={nodeZone} />
              </li>
            ))}
          </ul>
          <p>
            Cloudflare lists them on this tunnel, and pco does not run them on this node: whoever runs one holds the tunnel&apos;s token and gets a share
            of the requests to every hostname of the tunnel. If they are not yours, give the tunnel a new secret; that cuts every connector but pco&apos;s.
          </p>
        </>
      )}
      {refused && (
        <p>
          Cloudflare refuses the token the connector on this node runs with, as after the tunnel&apos;s secret was rotated. pco reads the token again
          every five minutes; a new secret gives the connector a token Cloudflare takes.
        </p>
      )}
      <CopyCommand cmd={rotateCommand(st.tunnels, t.accountId)} root />
      {observeOnly && <p className="muted">The daemon refuses the rotation for now: {observeOnlyRefusal}.</p>}
    </section>
  )
}

// TunnelDetail is the tunnel of the install in one account: how the last
// cycle found it, the configuration rolled out, its connector and traffic,
// the connectors pco does not run, and its events, found by the account as
// every tunnel of an install has the same name. It is the page of the tunnel
// and the tunnel's drawer of the map.
export function TunnelDetail({ accountId, variant }: { accountId: string; variant: 'drawer' | 'page' }): JSX.Element {
  const st = useApp((s) => s.state)
  const traffic = useApp((s) => s.traffic)
  const observeOnly = useApp((s) => s.settings?.settings.observeOnly ?? s.state?.mode === 'observe')
  if (!st) return <Skeleton lines={4} label="Loading the state" />
  const t = st.tunnels.find((x) => x.accountId === accountId)
  if (!t) return <p className="muted">The install has no tunnel in account <Untrusted text={accountId} /> in the last cycle.</p>
  const status = connectorOf(st, t)
  const tt = t.id ? traffic?.tunnels.find((x) => x.tunnelId === t.id) : undefined
  const compact = variant === 'drawer'

  return (
    <div className={compact ? 'detail detail-drawer' : 'detail'}>
      <Card id={`tunnel-${t.accountId}`} title={<Untrusted text={tunnelLabel(st, t)} />}>
        <dl className="details">
          <dt>Account</dt>
          <dd>
            <Untrusted text={accountName(st, t.accountId)} />{' '}
            <span className="muted mono">
              <Untrusted text={t.accountId} />
            </span>
          </dd>
          <dt>Name</dt>
          <dd className="mono">
            <Untrusted text={t.name} />
          </dd>
          <dt>Id</dt>
          <dd className="mono">{t.id ? <Untrusted text={t.id} /> : '-'}</dd>
          <dt>Credential</dt>
          <dd>
            <Untrusted text={credentialLabel(st, t.credentialId)} />
          </dd>
          <dt>Configuration</dt>
          <dd>version {t.version}</dd>
          <dt>Verified</dt>
          <dd>
            <Untrusted text={verifiedLine(t, st.hold)} />
          </dd>
          <dt>Rollout</dt>
          <dd>{rolloutText(t.rollout)}</dd>
        </dl>
        <Rogues st={st} t={t} observeOnly={observeOnly} />
      </Card>
      <Card id={`tunnel-connector-${t.accountId}`} title="Connector">
        <Connector status={status} traffic={tt} />
        {tt && traffic && (
          <TrafficChart
            label="Requests and errors"
            end={traffic.at}
            stale={tt.stale}
            series={[
              { name: 'requests', unit: 'req/s', samples: tt.samples.map((x) => ({ at: x.at, value: x.rps })) },
              { name: 'errors', unit: 'req/s', samples: tt.samples.map((x) => ({ at: x.at, value: x.errorsPerSec })) },
            ]}
          />
        )}
      </Card>
      <Card id={`tunnel-events-${t.accountId}`} title="Events">
        <EventsTable filter={{ account: [t.accountId] }} live rows={compact ? 5 : 10} />
      </Card>
    </div>
  )
}
