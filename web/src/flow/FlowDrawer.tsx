import type { ReactNode } from 'react'

import { useApp } from '../api/store'
import type { State, TrafficView } from '../api/types.gen'
import { CopyCommand } from '../components/CopyCommand'
import { Drawer } from '../components/Drawer'
import { Time } from '../components/Time'
import { TrafficChart } from '../components/TrafficChart'
import { Untrusted } from '../components/Untrusted'
import { Link } from '../app/Link'
import { GuestDetail } from '../pages/guests/GuestDetail'
import { TunnelDetail, tunnelLabel, tunnelPath } from '../pages/edge/TunnelDetail'
import { ZoneDetail } from '../pages/edge/ZoneDetail'
import { RouteDetail } from '../pages/routes/RouteDetail'
import { RouteTraffic } from '../pages/routes/RouteTraffic'
import { drawerWords as words } from '../text/flow'
import { rotateCommand } from '../text/words'
import { ChainList } from './ChainList'
import type { FlowEdge, FlowNode, FlowRow, Model } from './types'

interface Shown {
  title: ReactNode
  body: ReactNode
}

const keyOf = (r: Pick<FlowRow, 'hostname' | 'owner'>) => `${r.hostname} ${r.owner}`

function routesOf(st: State, traffic: TrafficView | undefined, keys: readonly string[], label: string): ReactNode {
  return <ChainList state={st} traffic={traffic} only={new Set(keys)} height={480} label={label} />
}

function routeOf(st: State, key: string) {
  return st.routes.find((r) => keyOf(r) === key)
}

function rowShown(st: State, traffic: TrafficView | undefined, r: FlowRow): Shown {
  if (r.kind === 'group' || r.kind === 'more') {
    return { title: <Untrusted text={r.label ?? ''} />, body: routesOf(st, traffic, r.routes ?? [], words.theirRoutes) }
  }
  if (r.kind === 'unapproved') return { title: <Untrusted text={r.hostname} hostname />, body: <GuestDetail key={r.owner} guest={r.owner} variant="drawer" /> }
  return { title: <Untrusted text={r.hostname} hostname />, body: <RouteDetail key={keyOf(r)} hostname={r.hostname} owner={r.owner} variant="drawer" /> }
}

function Rogue({ st, id }: { st: State; id: string }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const r = st.rogueConnectors.find((x) => x.id === id)
  if (!r) return <p className="muted">{words.gone}</p>
  const t = st.tunnels.find((x) => x.accountId === r.accountId)
  return (
    <div className="detail detail-drawer">
      <dl className="details">
        <dt>{words.connectorId}</dt>
        <dd className="mono">
          <Untrusted text={r.id} />
        </dd>
        <dt>{words.from}</dt>
        <dd className="mono">{r.originIp ? <Untrusted text={r.originIp} /> : words.unknownAddress}</dd>
        <dt>{words.cloudflared}</dt>
        <dd>{r.version ? <Untrusted text={r.version} /> : words.unknownVersion}</dd>
        <dt>{words.tunnel}</dt>
        <dd>{t ? <Link to={tunnelPath(t.accountId)}>{<Untrusted text={tunnelLabel(st, t)} />}</Link> : <Untrusted text={r.tunnel} />}</dd>
        <dt>{words.firstSeen}</dt>
        <dd>
          <Time at={r.since} nodeZone={nodeZone} />
        </dd>
      </dl>
      <p>{words.rogue}</p>
      <CopyCommand cmd={rotateCommand(st.tunnels, r.accountId)} root />
    </div>
  )
}

function nodeShown(st: State, traffic: TrafficView | undefined, n: FlowNode): Shown {
  const label = <Untrusted text={n.label} />
  switch (n.kind) {
    case 'zone':
      if (n.ref) return { title: <Untrusted text={n.ref} hostname />, body: <ZoneDetail key={n.ref} zone={n.ref} variant="drawer" /> }
      return { title: label, body: routesOf(st, traffic, n.routes ?? [], words.noZone) }
    case 'edge':
    case 'connector':
      return { title: label, body: <TunnelDetail key={n.ref} accountId={n.ref ?? ''} variant="drawer" /> }
    case 'rogue':
      return { title: label, body: <Rogue st={st} id={n.ref ?? ''} /> }
    case 'target': {
      if (n.id.startsWith('guest:') && n.ref) return { title: label, body: <GuestDetail key={n.ref} guest={n.ref} variant="drawer" /> }
      const [only] = n.routes ?? []
      const r = only !== undefined && n.routes?.length === 1 ? routeOf(st, only) : undefined
      if (r) return { title: <Untrusted text={r.hostname} hostname />, body: <RouteDetail key={only} hostname={r.hostname} owner={r.owner} variant="drawer" /> }
      return { title: label, body: routesOf(st, traffic, n.routes ?? [], words.itsRoutes) }
    }
  }
  return { title: label, body: routesOf(st, traffic, n.routes ?? [], words.theirRoutes) }
}

function Trunk({ st, traffic, edge }: { st: State; traffic?: TrafficView; edge: FlowEdge }) {
  const account = edge.from.slice('edge:'.length)
  const t = st.tunnels.find((x) => x.accountId === account)
  const tt = t?.id ? traffic?.tunnels.find((x) => x.tunnelId === t.id) : undefined
  const last = tt?.samples.at(-1)
  return (
    <div className="detail detail-drawer">
      {tt && traffic ? (
        <TrafficChart
          label={words.chart}
          end={traffic.at}
          stale={tt.stale}
          series={[
            { name: 'requests', unit: 'req/s', samples: tt.samples.map((x) => ({ at: x.at, value: x.rps })) },
            { name: 'errors', unit: 'req/s', samples: tt.samples.map((x) => ({ at: x.at, value: x.errorsPerSec })) },
          ]}
        />
      ) : (
        <p className="muted">{words.noFigures}</p>
      )}
      {last && <p className="num">{words.inFlight(last.concurrent)}</p>}
      <p className="muted">{words.counters}</p>
      {t && (
        <p>
          <Link to={tunnelPath(t.accountId)}>{words.theTunnel}</Link>
        </p>
      )}
    </div>
  )
}

function edgeShown(st: State, traffic: TrafficView | undefined, model: Model, e: FlowEdge): Shown | undefined {
  if (e.style === 'trunk') {
    const t = st.tunnels.find((x) => `edge:${x.accountId}` === e.from)
    return { title: t ? <Untrusted text={tunnelLabel(st, t)} /> : words.trunk, body: <Trunk st={st} traffic={traffic} edge={e} /> }
  }
  const to = model.nodes.find((n) => n.id === e.to)
  if (to?.band !== 'targets') return undefined
  const port = to.ports?.find((p) => p.id === e.port)
  const r = (e.routes ?? []).map((k) => routeOf(st, k)).find((x) => x !== undefined)
  const title = port?.target ? <Untrusted text={port.target} /> : <Untrusted text={to.label} />
  if (!r) return { title, body: <p className="muted">{words.noRoute}</p> }
  return { title, body: <RouteTraffic route={r} /> }
}

// pick is what the drawer shows for what was picked on the map: a line of a
// zone card, a card, the trunk or a line to a target.
function pick(st: State, traffic: TrafficView | undefined, model: Model, id: string): Shown | undefined {
  for (const n of model.nodes) {
    if (n.id === id) return nodeShown(st, traffic, n)
    const r = n.rows?.find((x) => (x.id ?? `route:${keyOf(x)}`) === id)
    if (r) return rowShown(st, traffic, r)
  }
  const e = model.edges.find((x) => x.id === id)
  return e && edgeShown(st, traffic, model, e)
}

export interface FlowDrawerProps {
  model?: Model // the view the map draws
  selected?: string
  onClose(): void
}

// FlowDrawer is the drawer of the map: the details of a route, a guest, a
// zone or a tunnel as their own pages show them, the traffic of a trunk or
// of a target, a connector pco does not run and the command that cuts it
// off, or the routes behind a card in the chain list.
export function FlowDrawer({ model, selected, onClose }: FlowDrawerProps) {
  const st = useApp((s) => s.state)
  const traffic = useApp((s) => s.traffic)
  const shown = st && model && selected ? pick(st, traffic, model, selected) : undefined
  return (
    <Drawer open={shown !== undefined} onClose={onClose} title={shown?.title ?? ''}>
      {shown?.body}
    </Drawer>
  )
}
