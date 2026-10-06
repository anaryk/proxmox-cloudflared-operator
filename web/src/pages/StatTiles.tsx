import { Fragment, type ReactNode } from 'react'

import type { State, TrafficView } from '../api/types.gen'
import { Link } from '../app/Link'
import { StateBadge } from '../components/StateBadge'
import { Time } from '../components/Time'
import { type Sample, TrafficChart } from '../components/TrafficChart'
import { Untrusted } from '../components/Untrusted'
import { firstCycleDone } from '../flow/model'
import { compareRouteStates } from '../text/words'

const waiting = 'waiting for the first cycle'

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

function Tile({ title, big, unit, children }: { title: string; big: ReactNode; unit?: ReactNode; children?: ReactNode }) {
  return (
    <section className="card tile" aria-label={title}>
      <h2 className="tile-title">{title}</h2>
      <p className="tile-big num">
        {big}
        {unit && <small> {unit}</small>}
      </p>
      {children}
    </section>
  )
}

// The requests and errors of every tunnel, summed per round of scrapes:
// the daemon stamps each round's samples with the same time.
function sums(traffic: TrafficView): { requests: Sample[]; errors: Sample[] } {
  const at = new Map<string, { rps: number; errors: number }>()
  for (const t of traffic.tunnels) {
    for (const s of t.samples) {
      const sum = at.get(s.at) ?? { rps: 0, errors: 0 }
      sum.rps += s.rps
      sum.errors += s.errorsPerSec
      at.set(s.at, sum)
    }
  }
  const times = [...at.keys()].sort()
  return {
    requests: times.map((t) => ({ at: t, value: at.get(t)?.rps ?? 0 })),
    errors: times.map((t) => ({ at: t, value: at.get(t)?.errors ?? 0 })),
  }
}

function RoutesTile({ state }: { state: State }) {
  if (!firstCycleDone(state)) {
    return (
      <Tile title="Routes" big="–">
        <p className="tile-sub">{waiting}</p>
      </Tile>
    )
  }
  const counts = new Map<string, number>()
  for (const r of state.routes) counts.set(r.state, (counts.get(r.state) ?? 0) + 1)
  const others = [...counts.keys()].filter((s) => s !== 'active').sort(compareRouteStates)
  return (
    <Tile title="Routes" big={counts.get('active') ?? 0} unit={`of ${state.routes.length} active`}>
      {others.length > 0 && (
        <ul className="tile-sub">
          {others.map((s) => (
            <li key={s}>
              <StateBadge state={s} /> <b className="num">{counts.get(s)}</b>
            </li>
          ))}
        </ul>
      )}
    </Tile>
  )
}

function TrafficTile({ state, traffic, nodeZone }: { state: State; traffic?: TrafficView; nodeZone?: string }) {
  if (!firstCycleDone(state)) {
    return (
      <Tile title="Edge traffic" big="–" unit="req/s">
        <p className="tile-sub">{waiting}</p>
      </Tile>
    )
  }
  const tunnels = traffic?.tunnels.filter((t) => t.samples.length > 0) ?? []
  if (!traffic || tunnels.length === 0) {
    return (
      <Tile title="Edge traffic" big="–" unit="req/s">
        <p className="tile-sub">No figures from the connectors yet.</p>
      </Tile>
    )
  }
  const fresh = tunnels.filter((t) => !t.stale)
  const latest = (pick: (s: { rps: number; errorsPerSec: number; concurrent: number }) => number) =>
    fresh.reduce((sum, t) => sum + pick(t.samples.at(-1) ?? { rps: 0, errorsPerSec: 0, concurrent: 0 }), 0)
  const stale = fresh.length === 0
  const lastSample = tunnels
    .map((t) => t.samples.at(-1)?.at ?? '')
    .sort()
    .at(-1)
  const { requests, errors } = sums(traffic)
  return (
    <Tile title="Edge traffic" big={stale ? '–' : latest((s) => s.rps).toFixed(1)} unit="req/s">
      <TrafficChart
        label="Requests to the connectors"
        end={traffic.at}
        stale={stale}
        series={[
          { name: 'requests', unit: 'req/s', samples: requests },
          { name: 'errors', unit: 'errors/s', samples: errors },
        ]}
      />
      <p className="tile-sub num">
        {stale ? (
          <span>no data since {lastSample ? <Time at={lastSample} nodeZone={nodeZone} /> : 'the connectors started'}</span>
        ) : (
          <>
            <span>{latest((s) => s.errorsPerSec).toFixed(1)} errors/s</span> <span>{plural(latest((s) => s.concurrent), 'request')} in flight</span>
          </>
        )}
      </p>
    </Tile>
  )
}

function ConnectorsTile({ state, traffic }: { state: State; traffic?: TrafficView }) {
  if (!firstCycleDone(state)) {
    return (
      <Tile title="Connectors" big="–">
        <p className="tile-sub">{waiting}</p>
      </Tile>
    )
  }
  const ready = state.connectors.filter((c) => c.active && c.ready).length
  const byTunnel = new Map((traffic?.tunnels ?? []).map((t) => [t.tunnelId, t]))
  const connections = state.connectors.reduce((sum, c) => sum + (byTunnel.get(c.tunnelId)?.haConnections ?? c.connections), 0)
  const at = new Map<string, number>()
  for (const c of state.connectors) for (const e of byTunnel.get(c.tunnelId)?.edges ?? []) at.set(e.location, (at.get(e.location) ?? 0) + 1)
  const rogue = state.rogueConnectors.length
  return (
    <Tile title="Connectors" big={ready} unit={`of ${state.connectors.length} ready`}>
      <ul className="tile-sub">
        <li>{plural(connections, 'connection')}</li>
        {at.size > 0 && (
          <li className="mono">
            {[...at]
              .sort((a, b) => (a[0] < b[0] ? -1 : 1))
              .map(([loc, n], i) => (
                <Fragment key={loc}>
                  {i > 0 && ' · '}
                  <Untrusted text={loc} />
                  {n > 1 && ` ×${n}`}
                </Fragment>
              ))}
          </li>
        )}
        {rogue > 0 && (
          <li className="status-fail">
            <Link to="/edge/tunnels">{plural(rogue, 'connector')} not run by pco</Link>
          </li>
        )}
      </ul>
    </Tile>
  )
}

// What needs a person, each with the page where it is dealt with. The
// problems are in the card under the tiles.
function NeedsTile({ state }: { state: State }) {
  const items: [number, string, string?][] = [
    [state.problems.length, plural(state.problems.length, 'problem')],
    [state.waiting.length, `${plural(state.waiting.length, 'confirmation')} waiting`, '/routes/plan'],
    [state.unapproved.length, `${plural(state.unapproved.length, 'guest')} waiting for approval`, '/guests'],
    [state.conflicts.length, `${plural(state.conflicts.length, 'DNS record')} in the way`, '/routes/plan'],
    [state.lost.length, plural(state.lost.length, 'lost name'), '/routes/plan'],
  ]
  const shown = items.filter(([n]) => n > 0)
  const total = shown.reduce((sum, [n]) => sum + n, 0)
  return (
    <Tile title="Needs you" big={total} unit={total === 1 ? 'item' : 'items'}>
      {shown.length === 0 ? (
        <p className="tile-sub">Nothing needs you.</p>
      ) : (
        <ul className="tile-sub">
          {shown.map(([, text, to]) => (
            <li key={text}>{to ? <Link to={to}>{text}</Link> : text}</li>
          ))}
        </ul>
      )}
    </Tile>
  )
}

// StatTiles is the row of figures at the top of the Overview: the routes by
// state, the requests the tunnels carry, the connectors, and what needs a
// person. Before the first cycle they wait for it.
export function StatTiles({ state, traffic, nodeZone }: { state: State; traffic?: TrafficView; nodeZone?: string }) {
  return (
    <div className="tiles">
      <RoutesTile state={state} />
      <TrafficTile state={state} traffic={traffic} nodeZone={nodeZone} />
      <ConnectorsTile state={state} traffic={traffic} />
      <NeedsTile state={state} />
    </div>
  )
}
