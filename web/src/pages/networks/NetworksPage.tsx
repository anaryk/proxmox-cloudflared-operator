import './networks.css'

import { useMemo, useState } from 'react'

import { useApp } from '../../api/store'
import type { RouteView, SegmentView } from '../../api/types.gen'
import { Head } from '../../app/Head'
import { Link } from '../../app/Link'
import { explain } from '../../api/errors'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Drawer } from '../../components/Drawer'
import { Skeleton } from '../../components/Skeleton'
import { StateBadge } from '../../components/StateBadge'
import { type Column, Table } from '../../components/Table'
import { Time } from '../../components/Time'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { asApiError, useAdminReason } from '../kit'
import { guestsOf, levelsOf, type Network, networksOf } from './networks'
import { acknowledgeSegment, revokeSegment, segmentName } from './segments'

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
// guests of it. There is no subnet: nothing in the daemon knows one. A route
// reached through the leg of a network has no row; a line counts them.
function DirectTable({ routes, nodeZone }: { routes: RouteView[]; nodeZone?: string }) {
  const { networks, unproven, throughLegs } = useMemo(() => networksOf(routes), [routes])
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
        empty="No route has been proven directly on a bridge yet. A route shows here once a cycle has proven the address of its guest, or when it is a manual route."
      />
      {throughLegs > 0 && (
        <p className="muted">
          {plural(throughLegs, 'route is', 'routes are')} reached through the leg of {throughLegs === 1 ? 'a network' : 'networks'}, not directly, so {throughLegs === 1 ? 'it is' : 'they are'} in no row. The
          routes page says how.
        </p>
      )}
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
  [
    'filtered',
    'in the appliance, a guest on the managed network pconet: only the guest answers ARP for its address, asked from pco’s leg, and the Proxmox firewall of every card on the vnet is proven to pin it to its address and keep it from everything but the leg.',
  ],
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

function SegmentAction({ s }: { s: SegmentView }) {
  const toast = useToast()
  const refusal = useAdminReason()
  const [sending, setSending] = useState(false)
  const send = async () => {
    setSending(true)
    try {
      if (s.acknowledged) {
        await revokeSegment(s)
        toast(
          <>
            The acknowledgement of segment <Untrusted text={segmentName(s)} /> is revoked; its routes at observed are held from the next cycle.
          </>,
          'ok',
        )
      } else {
        await acknowledgeSegment(s)
        toast(
          <>
            Segment <Untrusted text={segmentName(s)} /> is acknowledged; its routes at observed are served from the next cycle.
          </>,
          'ok',
        )
      }
    } catch (e) {
      const said = explain(asApiError(e))
      toast(said.quoted ? <Untrusted text={said.text} /> : said.text, 'fail')
    } finally {
      setSending(false)
    }
  }
  return (
    <Button small disabled={sending} disabledReason={refusal} onClick={() => void send()}>
      {s.acknowledged ? 'Revoke' : 'Acknowledge'}
    </Button>
  )
}

function Segments({ segments, nodeZone }: { segments: SegmentView[]; nodeZone?: string }) {
  if (segments.length === 0) return null
  const columns: Column<SegmentView>[] = [
    { key: 'bridge', header: 'Bridge', lead: true, cell: (s) => <Untrusted text={s.bridge} /> },
    { key: 'vlan', header: 'VLAN', cell: (s) => (s.vlan === undefined ? <span className="muted">untagged</span> : <span className="num">{s.vlan}</span>) },
    { key: 'acknowledged', header: 'Acknowledged', cell: (s) => (s.acknowledged ? <Time at={s.acknowledgedAt ?? ''} nodeZone={nodeZone} /> : <span className="muted">no</span>) },
    { key: 'routes', header: 'Routes', cell: (s) => <span className="num">{s.routes}</span> },
    { key: 'note', header: 'Note', cell: (s) => (s.own ? "an appliance's card" : <span className="muted">-</span>) },
    { key: 'action', header: 'Action', cell: (s) => <SegmentAction s={s} /> },
  ]
  return (
    <section className="card" aria-labelledby="networks-segments">
      <div className="card-head">
        <h2 id="networks-segments">Segments of routes at observed</h2>
      </div>
      <div className="card-body">
        <p>
          A route proven at <b>observed</b> is served only on a segment, a bridge and a VLAN, that an admin acknowledged: on a new one nothing is served until then. Acknowledge
          one once you would trust any device on it with what its MAC can claim. The segments of the appliances&apos; own cards are listed before any route is proven on them.
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
