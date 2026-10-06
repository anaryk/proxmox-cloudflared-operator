import './networks.css'

import { useMemo, useState } from 'react'

import { useApp } from '../../api/store'
import type { RouteView, SegmentView } from '../../api/types.gen'
import { Head } from '../../app/Head'
import { Link } from '../../app/Link'
import { Badge } from '../../components/Badge'
import { Drawer } from '../../components/Drawer'
import { Skeleton } from '../../components/Skeleton'
import { StateBadge } from '../../components/StateBadge'
import { type Column, Table } from '../../components/Table'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { guestsOf, levelsOf, type Network, networksOf } from './networks'

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`

const vlanText = (vlan: number | undefined) => (vlan === undefined ? 'untagged' : `VLAN ${vlan}`)

function RouteItem({ r, nodeZone }: { r: RouteView; nodeZone?: string }) {
  return (
    <li>
      <Link to={`/routes/${encodeURIComponent(r.hostname)}?owner=${encodeURIComponent(r.owner)}`}>
        <Untrusted text={r.hostname} hostname />
      </Link>{' '}
      <StateBadge state={r.state} />
      {r.level && (
        <>
          {' '}
          <Badge outline>
            <Untrusted text={r.level} />
          </Badge>
        </>
      )}
      <div className="muted">
        <Untrusted text={r.owner} />
        {r.path?.port && (
          <>
            , port <Untrusted text={r.path.port} />
          </>
        )}
        {r.path?.verifiedAt && (
          <>
            , verified <Time at={r.path.verifiedAt} nodeZone={nodeZone} />
          </>
        )}
      </div>
    </li>
  )
}

function NetworkDetail({ n, nodeZone, onClose }: { n: Network; nodeZone?: string; onClose: () => void }) {
  const guests = guestsOf(n.routes)
  return (
    <Drawer
      open
      onClose={onClose}
      title={n.bridge ? <Untrusted text={`${n.bridge}, ${vlanText(n.vlan)}`} /> : 'No bridge proven'}
    >
      <div className="network-detail">
        {n.bridge ? (
          <p>The VLAN is the tag the guests&apos; network cards are configured with. The proof did not see it.</p>
        ) : (
          <p>
            These routes were not proven on a bridge of this node: manual routes, which name an address and no guest, and routes whose proof placed no MAC on a bridge, such as an
            address behind a router.
          </p>
        )}
        <h3>{plural(n.routes.length, 'route', 'routes')}</h3>
        <ul className="network-list">
          {n.routes.map((r) => (
            <RouteItem key={`${r.hostname}:${r.owner}`} r={r} nodeZone={nodeZone} />
          ))}
        </ul>
        <h3>{plural(guests.length, 'guest', 'guests')}</h3>
        {guests.length === 0 ? (
          <p className="muted">None: no route here belongs to a guest.</p>
        ) : (
          <ul className="network-list">
            {guests.map((g) => (
              <li key={g.ref}>
                {g.name && (
                  <>
                    <Untrusted text={g.name} />{' '}
                  </>
                )}
                <span className="muted">
                  <Untrusted text={g.ref} />
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </Drawer>
  )
}

const columns: Column<Network>[] = [
  {
    key: 'bridge',
    header: 'Bridge',
    lead: true,
    cell: (n) => (n.bridge ? <Untrusted text={n.bridge} /> : <span className="muted">no bridge proven</span>),
  },
  {
    key: 'vlan',
    header: 'VLAN',
    cell: (n) => (!n.bridge ? <span className="muted">-</span> : n.vlan === undefined ? <span className="muted">untagged</span> : <span className="num">{n.vlan}</span>),
  },
  { key: 'routes', header: 'Routes', cell: (n) => <span className="num">{n.routes.length}</span> },
  {
    key: 'levels',
    header: 'Levels',
    cell: (n) => (
      <span className="levels">
        {levelsOf(n.routes).map(([level, count]) => (
          <Badge key={level} outline>
            <Untrusted text={level} /> <span className="num">{count}</span>
          </Badge>
        ))}
      </span>
    ),
  },
  { key: 'guests', header: 'Guests', cell: (n) => <span className="num">{guestsOf(n.routes).length}</span> },
]

// The table of the networks the routes were proven on, a row for each bridge
// and VLAN, and the one for routes proven on none; a row opens the routes and
// guests of it. There is no subnet: nothing in the daemon knows one.
function DirectTable({ routes, nodeZone }: { routes: RouteView[]; nodeZone?: string }) {
  const { networks, unproven } = useMemo(() => networksOf(routes), [routes])
  const [open, setOpen] = useState<string>()
  const shown = networks.find((n) => n.key === open)
  return (
    <>
      <Table
        label="Direct networks"
        columns={columns}
        rows={networks}
        rowKey={(n) => n.key}
        current={open}
        onActivate={(n) => setOpen(n.key)}
        empty="No route has been proven on a bridge yet. A route shows here once a cycle has proven the address of its guest, or when it is a manual route."
      />
      {unproven > 0 && (
        <p className="muted">
          {plural(unproven, 'route is', 'routes are')} in no row: none of {unproven === 1 ? 'its' : 'their'} addresses was proven, because{' '}
          {unproven === 1 ? 'it' : 'they'} lost the hostname, {unproven === 1 ? 'was' : 'were'} refused or {unproven === 1 ? 'is' : 'are'} held. The routes page says why.
        </p>
      )}
      {shown && <NetworkDetail n={shown} nodeZone={nodeZone} onClose={() => setOpen(undefined)} />}
    </>
  )
}

const levelWords: [string, string][] = [
  ['port', 'only the guest answers ARP for its address, and the forwarding table of this node places each MAC of the guest on the guest’s own port.'],
  [
    'observed',
    'only the guest answers ARP for its address, but no table placed it on a port (a guest on another node), or the address is a trusted static one that only the guest’s configuration vouches for.',
  ],
  ['filtered', 'reserved for a network where the firewall pins every card of a guest to its address. Nothing proves it in this release.'],
  ['manual', 'a route that names an address and no guest. The admin who wrote it vouches for it; nothing proves it.'],
]

function Identity() {
  const minimum = useApp((s) => s.settings?.settings.identityMinimum)
  return (
    <section className="card" aria-labelledby="networks-identity">
      <div className="card-head">
        <h2 id="networks-identity">How a route is proven</h2>
      </div>
      <div className="card-body">
        <dl className="details">
          {levelWords.map(([level, words]) => (
            <div key={level} className="details-row">
              <dt>
                <b>{level}</b>
              </dt>
              <dd>{words}</dd>
            </div>
          ))}
        </dl>
        <p>
          pco serves a route only at <b>identityMinimum</b> or higher
          {minimum ? (
            <>
              : here it is <b>{minimum}</b>
            </>
          ) : null}
          . It is a setting of the whole install; <Link to="/settings">Settings</Link> has it, and the identity guide says what each level protects against.
        </p>
      </div>
    </section>
  )
}

function Segments({ segments, nodeZone }: { segments: SegmentView[]; nodeZone?: string }) {
  if (segments.length === 0) return null
  const columns: Column<SegmentView>[] = [
    { key: 'bridge', header: 'Bridge', lead: true, cell: (s) => <Untrusted text={s.bridge} /> },
    { key: 'vlan', header: 'VLAN', cell: (s) => (s.vlan === undefined ? <span className="muted">untagged</span> : <span className="num">{s.vlan}</span>) },
    { key: 'acknowledged', header: 'Acknowledged', cell: (s) => (s.acknowledged ? <Time at={s.acknowledgedAt ?? ''} nodeZone={nodeZone} /> : <span className="muted">no</span>) },
    { key: 'routes', header: 'Routes', cell: (s) => <span className="num">{s.routes}</span> },
  ]
  return (
    <section className="card" aria-labelledby="networks-segments">
      <div className="card-head">
        <h2 id="networks-segments">Segments of routes at observed</h2>
      </div>
      <div className="card-body">
        <p>
          A route proven at <b>observed</b> is served only on a segment, a bridge and a VLAN, that an admin acknowledged: on a new one nothing is served until then. On the node,{' '}
          <code>pco segment list</code> shows the segments and <code>pco segment acknowledge</code> acknowledges one.
        </p>
        <Table label="Segments" columns={columns} rows={segments} rowKey={(s) => `${s.bridge}:${s.vlan ?? ''}`} />
      </div>
    </section>
  )
}

// NetworksPage says where the routes were proven. Its sections are cards of
// their own, so that the networking milestone adds its Networks table and its
// Managed card after Direct without moving what is here.
export function NetworksPage() {
  const state = useApp((s) => s.state)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  return (
    <>
      <Head title="Networks" description="The bridges and VLANs the routes were proven on." />
      {state ? (
        <div className="networks">
          <section className="card" aria-labelledby="networks-direct">
            <div className="card-head">
              <h2 id="networks-direct">Direct</h2>
            </div>
            <div className="card-body">
              <p>
                With Direct, pco reaches a guest on the network the node itself is on: the node has an address of its own on the bridge, and the VLAN, of the guest&apos;s network card, and
                sends to the guest&apos;s address without a gateway. The table says where each route was proven. It shows no subnet: pco does not know the subnets of a network.
              </p>
              <DirectTable routes={state.routes} nodeZone={nodeZone} />
            </div>
          </section>
          <Segments segments={state.segments} nodeZone={nodeZone} />
          <Identity />
        </div>
      ) : (
        <Skeleton lines={4} label="Loading the networks" />
      )}
    </>
  )
}
