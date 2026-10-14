import { Fragment, type JSX, useState } from 'react'

import { useApp } from '../../api/store'
import type { RouteView, State } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Empty } from '../../components/Empty'
import { Skeleton } from '../../components/Skeleton'
import { StateBadge, StatusBadge } from '../../components/StateBadge'
import { Tabs } from '../../components/Tabs'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { tunnelPath } from '../edge/TunnelDetail'
import { moveText, nodeLink, replicaLevelText, replicaTone } from '../nodes/nodes'
import { AdoptDialog, inTheWay } from './AdoptDialog'
import { ClaimPanel } from './ClaimPanel'
// The extensions are named: diagnosis.ts sits beside Diagnosis.tsx, and a
// file system that ignores case would take one for the other.
import { Diagnosis } from './Diagnosis.tsx'
import { NoteLine, Owner, readerReason, useAdmin } from './parts'
import { LevelOf } from './RouteTable'
import { RouteTraffic } from './RouteTraffic'
import { allowHostLink, holderOf, isManual, isWildcard, manualPrefix, openableURL, pathText, reachText, routeLink, unset } from './routes'
import { Timeline } from './Timeline'

export const diagnosisTab = 'Diagnosis of the current holder'

function tunnelOf(st: State, route: RouteView) {
  const p = route.placement
  if (p?.kind === 'pinned') return st.tunnels.find((t) => t.kind === 'pinned' && t.name === p.tunnel && t.accountId === route.accountId)
  return st.tunnels.find((t) => t.kind !== 'pinned' && t.accountId === route.accountId)
}

function accountName(st: State, id: string): string | undefined {
  for (const c of st.credentials) {
    const found = c.report?.accounts.find((a) => a.id === id)
    if (found) return found.name
  }
  return undefined
}

function ownerLink(owner: string): string | undefined {
  if (isManual(owner)) return `/routes/manual/${encodeURIComponent(owner.slice(manualPrefix.length))}`
  return /^(qemu|lxc)\/[0-9]+$/.test(owner) ? `/guests/${owner}` : undefined
}

// elsewhere is the node that runs the guest of a route when the path was
// proven on another one: the port of that path is where the proving node
// sees the guest, its uplink, and never the guest's own port. The node is
// the inventory's, or else one that vouched for the guest's port.
function elsewhere(route: RouteView): string | undefined {
  const path = route.path
  if (!path?.port) return undefined
  const host = route.guestNode ?? (route.replicas ?? []).find((r) => r.attested && r.attestedBy)?.attestedBy
  return host && host !== path.node ? host : undefined
}

// PathLines are the path the route was proven on, how the node reaches the
// guest when that is through the leg of a network, when it was proven last,
// and since when the guest has been bound there.
function PathLines({ route, nodeZone }: { route: RouteView; nodeZone?: string }) {
  const path = route.path
  const reach = reachText(path)
  const host = elsewhere(route)
  return (
    <>
      <dt>Path</dt>
      <dd>
        <Untrusted text={pathText(path)} />
        {host && path && (
          <p className="field-hint">
            The guest runs on <Untrusted className="mono" text={host} />: <Untrusted className="mono" text={path.port ?? ''} /> is where{' '}
            <Untrusted className="mono" text={path.node} /> sees it, not the port of the guest.
          </p>
        )}
      </dd>
      {reach && (
        <>
          <dt>Via</dt>
          <dd>
            <Untrusted text={reach} />
          </dd>
        </>
      )}
      {path && !unset(path.verifiedAt) && (
        <>
          <dt>Last proof stored</dt>
          <dd>
            <Time at={path.verifiedAt} nodeZone={nodeZone} />
            <p className="field-hint">pco stores a proof only when it changed, so the last proof can be newer.</p>
          </dd>
        </>
      )}
      {path?.since && !unset(path.since) && (
        <>
          <dt>Bound since</dt>
          <dd>
            <Time at={path.since} nodeZone={nodeZone} />
          </dd>
        </>
      )}
    </>
  )
}

// ClusterLines are the placement of a route on a cluster, a move of it
// under way, and how its target fares on each replica of its tunnel: the
// level each proved, port (attested by pve1) where the forwarding table of
// the node that runs the guest vouched for the port, and what fails where.
function ClusterLines({ route, nodeZone }: { route: RouteView; nodeZone?: string }) {
  const p = route.placement
  const replicas = route.replicas ?? []
  if (!p && replicas.length === 0) return null
  return (
    <>
      {p && (
        <>
          <dt>Placement</dt>
          <dd>
            {p.kind === 'pinned' ? (
              <>
                pinned to{' '}
                {p.node ? (
                  <Link to={nodeLink(p.node)} className="mono">
                    <Untrusted text={p.node} />
                  </Link>
                ) : (
                  'a node'
                )}{' '}
                (tunnel <Untrusted className="mono" text={p.tunnel} />)
              </>
            ) : (
              <>
                shared tunnel <Untrusted className="mono" text={p.tunnel} />
              </>
            )}
            {p.why && (
              <p className="field-hint">
                <Untrusted text={p.why} />
              </p>
            )}
          </dd>
          {p.move && (
            <>
              <dt>Move</dt>
              <dd>
                <Untrusted text={moveText(p.move)} />
                {p.move.since && !unset(p.move.since) && (
                  <p className="field-hint">
                    Since <Time at={p.move.since} nodeZone={nodeZone} />.
                  </p>
                )}
              </dd>
            </>
          )}
        </>
      )}
      {replicas.length > 0 && (
        <>
          <dt>Replicas</dt>
          <dd>
            <ul className="replica-list" aria-label="Replicas">
              {replicas.map((r) => (
                <li key={r.node}>
                  <Link to={nodeLink(r.node)} className="mono">
                    <Untrusted text={r.node} />
                  </Link>
                  <Badge outline>
                    <Untrusted text={replicaLevelText(r, route.guestNode)} />
                  </Badge>
                  <StatusBadge tone={replicaTone(r)}>{r.ok ? (r.inSet ? 'reached, in its set' : 'reached, not in its set yet') : 'not reached'}</StatusBadge>
                  {r.reason && (
                    <span className="muted">
                      <Untrusted text={r.reason} />
                    </span>
                  )}
                </li>
              ))}
            </ul>
            <p className="field-hint">A route is placed in the shared tunnel only while every replica reaches its target at or above the identity minimum and holds it in its egress set.</p>
          </dd>
        </>
      )}
    </>
  )
}

function Overview({ route, st, admin }: { route: RouteView; st: State; admin: boolean }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const allow = admin ? allowHostLink(route) : undefined
  const to = ownerLink(route.owner)
  const tunnel = route.accountId ? tunnelOf(st, route) : undefined
  const rule = route.rule
  const ruleRows: [string, string][] = rule
    ? ([
        ['service', rule.service],
        ['httpHostHeader', rule.httpHostHeader ?? ''],
        ['originServerName', rule.originServerName ?? ''],
        ['noTLSVerify', rule.noTLSVerify ? 'true' : ''],
        ['matchSNItoHost', rule.matchSNItoHost ? 'true' : ''],
      ].filter(([, v]) => v) as [string, string][])
    : []
  return (
    <div className="route-overview">
      {isWildcard(route.hostname) && (
        <NoteLine>A wildcard also catches every name of the zone that has no record of its own.</NoteLine>
      )}
      <dl className="details">
        <dt>State</dt>
        <dd>
          <StateBadge state={route.state} />
        </dd>
        {route.reason && (
          <>
            <dt>Reason</dt>
            <dd>
              <Untrusted text={route.reason} />
              {allow && (
                <p>
                  <Link to={allow} className="btn btn-small">
                    Add to allowHosts
                  </Link>
                </p>
              )}
            </dd>
          </>
        )}
        {(route.warnings ?? []).length > 0 && (
          <>
            <dt>Warnings</dt>
            <dd>
              <ul className="plain-list">
                {(route.warnings ?? []).map((w) => (
                  <li key={w}>
                    <Untrusted text={w} />
                  </li>
                ))}
              </ul>
            </dd>
          </>
        )}
        <dt>Owner</dt>
        <dd>
          {to ? (
            <Link to={to}>
              <Owner owner={route.owner} guest={route.guest} />
            </Link>
          ) : (
            <Owner owner={route.owner} guest={route.guest} />
          )}
        </dd>
        <dt>Zone</dt>
        <dd>{route.zone ? <Untrusted text={route.zone} /> : <span className="muted">none</span>}</dd>
        <dt>Service</dt>
        <dd className="mono">{route.service ? <Untrusted text={route.service} /> : <span className="muted">none</span>}</dd>
        <dt>Level</dt>
        <dd>
          <LevelOf route={route} />
        </dd>
        {ruleRows.length > 0 && (
          <>
            <dt>Planned rule</dt>
            <dd>
              <dl className="details details-inner mono">
                {ruleRows.map(([k, v]) => (
                  <Fragment key={k}>
                    <dt>{k}</dt>
                    <dd>
                      <Untrusted text={v} />
                    </dd>
                  </Fragment>
                ))}
              </dl>
            </dd>
          </>
        )}
        {route.accountId && (
          <>
            <dt>Tunnel</dt>
            <dd>
              <Link to={tunnelPath(route.accountId, tunnel?.kind === 'pinned' ? tunnel.name : undefined)}>
                account <Untrusted text={accountName(st, route.accountId) ?? route.accountId} />
                {tunnel?.kind === 'pinned' ? (
                  <>
                    {' '}
                    · pinned to <Untrusted className="mono" text={tunnel.node ?? ''} />
                  </>
                ) : (
                  tunnel?.id && (
                    <>
                      {' '}
                      · <Untrusted className="mono" text={tunnel.id.slice(0, 8)} />
                    </>
                  )
                )}
              </Link>
            </dd>
          </>
        )}
        <PathLines route={route} nodeZone={nodeZone} />
        <ClusterLines route={route} nodeZone={nodeZone} />
        {(route.candidates ?? []).length > 0 && (
          <>
            <dt>Candidates</dt>
            <dd>
              <ul className="plain-list candidates">
                {(route.candidates ?? []).map((c) => (
                  <li key={`${c.addr}\u0000${c.source}`}>
                    <span className="mono">
                      <Untrusted text={c.addr} />
                    </span>{' '}
                    <Badge outline>
                      <Untrusted text={c.source} />
                    </Badge>{' '}
                    {c.ok ? <Badge tone="ok">ok</Badge> : <Badge tone="fail">not ok</Badge>}
                    {c.level && (
                      <>
                        {' '}
                        <span className="muted">
                          <Untrusted text={c.level} />
                        </span>
                      </>
                    )}
                    {c.reason && (
                      <>
                        {' '}
                        <Untrusted text={c.reason} />
                      </>
                    )}
                  </li>
                ))}
              </ul>
            </dd>
          </>
        )}
      </dl>
      <section className="route-section" aria-labelledby="route-traffic">
        <h3 id="route-traffic">Traffic of the target</h3>
        <RouteTraffic route={route} />
      </section>
    </div>
  )
}

// RouteDetail is a route: its state and why, its path and traffic, the
// diagnosis of the route that holds its hostname, its history and its
// claim. Without an owner it is the route that holds the hostname. In a
// drawer the drawer names the hostname; as a page it is the heading of the
// page.
export function RouteDetail(props: { hostname: string; owner?: string; variant: 'drawer' | 'page' }): JSX.Element {
  const { hostname, owner, variant } = props
  const st = useApp((s) => s.state)
  const admin = useAdmin()
  const [tab, setTab] = useState('overview')
  const [adopting, setAdopting] = useState(false)

  if (!st) return <Skeleton lines={6} label="Loading the route" />
  const same = st.routes.filter((r) => r.hostname.toLowerCase() === hostname.toLowerCase())
  const route = owner ? same.find((r) => r.owner === owner) : holderOf(st.routes, hostname)
  const title = variant === 'page' && (
    <h1 className="detail-title mono">
      <Untrusted text={hostname} hostname />
    </h1>
  )

  if (!route) {
    return (
      <div className={`route-detail route-detail-${variant}`}>
        {title}
        <Empty title={owner ? 'This route is gone' : 'No route asks for this hostname now'}>
          {owner && (
            <p>
              <Untrusted text={owner} /> asks for <Untrusted text={hostname} hostname /> no longer.
            </p>
          )}
          {same.length > 0 ? (
            <ul className="plain-list">
              {same.map((r) => (
                <li key={r.owner}>
                  <Link to={routeLink(r.hostname, r.owner)}>
                    <Owner owner={r.owner} guest={r.guest} />
                  </Link>{' '}
                  <StateBadge state={r.state} />
                </li>
              ))}
            </ul>
          ) : (
            <p>
              <Link to="/routes">All routes</Link>
            </p>
          )}
        </Empty>
      </div>
    )
  }

  const lost = route.state === 'conflict'
  // The route the daemon diagnoses: the one that holds the hostname, or the
  // first that asks for it when every one of them lost it.
  const holder = holderOf(st.routes, route.hostname)
  const diagnosed = holder?.owner === route.owner
  const others = same.filter((r) => r.owner !== route.owner)
  const tabs = [
    { id: 'overview', label: 'Overview' },
    ...(diagnosed ? [{ id: 'diagnosis', label: diagnosisTab }] : []),
    { id: 'timeline', label: 'Timeline' },
    { id: 'claim', label: 'Claim' },
  ]
  const selected = tabs.some((t) => t.id === tab) ? tab : 'overview'
  const open = openableURL(route.hostname)
  const way = inTheWay(st, route.hostname)
  const manual = isManual(route.owner)

  return (
    <div className={`route-detail route-detail-${variant}`}>
      {title}
      <p className="detail-summary">
        <StateBadge state={route.state} /> <Owner owner={route.owner} guest={route.guest} />
      </p>
      {others.length > 0 && (
        <p className="muted">
          Also asked for by{' '}
          {others.map((r, i) => (
            <Fragment key={r.owner}>
              {i > 0 && ', '}
              <Link to={routeLink(r.hostname, r.owner)}>
                <Untrusted text={r.owner} />
              </Link>{' '}
              (<Untrusted text={r.state} />)
            </Fragment>
          ))}
          .
        </p>
      )}
      {lost && holder && !diagnosed && (
        <NoteLine>
          This route lost its hostname to <Untrusted text={holder.owner} />. The daemon diagnoses the route that holds it:{' '}
          <Link to={routeLink(holder.hostname, holder.owner)}>
            the route of <Untrusted text={holder.owner} />
          </Link>
          .
        </NoteLine>
      )}
      {lost && diagnosed && (
        <NoteLine>
          {admin
            ? 'Every route that asks for this hostname lost it. The daemon diagnoses the first of them, this one.'
            : 'No route you can see holds this hostname. The daemon diagnoses the route that holds it, and runs the diagnosis for you only when that route is one you can see.'}
        </NoteLine>
      )}
      <div className="detail-actions">
        {open && (
          <a className="btn btn-small" href={open} target="_blank" rel="noopener noreferrer">
            Open {open}
          </a>
        )}
        {(way.conflict || way.lost) && (
          <Button small onClick={() => setAdopting(true)} disabledReason={admin ? undefined : readerReason}>
            {way.lost ? 'Take back' : 'Adopt'}
          </Button>
        )}
        {manual && admin && (
          <Link to={`/routes/manual/${encodeURIComponent(route.owner.slice(manualPrefix.length))}`} className="btn btn-small">
            Edit the manual route
          </Link>
        )}
      </div>
      <Tabs label="Route" tabs={tabs} selected={selected} onSelect={setTab}>
        {selected === 'overview' && <Overview route={route} st={st} admin={admin} />}
        {selected === 'diagnosis' && <Diagnosis hostname={route.hostname} />}
        {selected === 'timeline' && <Timeline hostname={route.hostname} />}
        {selected === 'claim' && <ClaimPanel hostname={route.hostname} />}
      </Tabs>
      {adopting && <AdoptDialog name={route.hostname} onClose={() => setAdopting(false)} />}
    </div>
  )
}
