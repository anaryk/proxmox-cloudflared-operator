import type { ReactNode } from 'react'

import type { RouteView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { Badge } from '../../components/Badge'
import { StateBadge } from '../../components/StateBadge'
import { type Column, Table } from '../../components/Table'
import { Untrusted } from '../../components/Untrusted'
import { compareRouteStates, routeNote } from '../../text/words'
import { Owner } from './parts'
import { allowHostLink, compareOwners, compareRoutes, isManual, isWildcard, routeKey } from './routes'

const compareText = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)

// HostCell is the hostname and, under it, the note of the route: its reason,
// else its first warning, as pco routes prints them. A rejected route offers
// an admin to add its hostname to allowHosts.
function HostCell({ route, admin }: { route: RouteView; admin: boolean }) {
  const note = routeNote(route)
  const allow = admin ? allowHostLink(route) : undefined
  return (
    <>
      <span className="cell-line mono">
        <Untrusted text={route.hostname} hostname />
        {isWildcard(route.hostname) && (
          <>
            {' '}
            <Badge tone="info">wildcard</Badge>
          </>
        )}
      </span>
      <span className="cell-line route-note">
        {note ? <Untrusted text={note} /> : null}
        {allow && (
          <Link to={allow} className="note-action">
            Add to allowHosts
          </Link>
        )}
      </span>
    </>
  )
}

export function LevelOf({ route }: { route: RouteView }) {
  if (route.level) {
    return (
      <Badge outline>
        <Untrusted text={route.level} />
      </Badge>
    )
  }
  if (isManual(route.owner)) return <Badge outline>manual</Badge>
  return <span className="muted">-</span>
}

const dash = <span className="muted">-</span>

// RouteTable is the table of the routes: windowed, sorted as pco routes
// sorts them, a row opening the route's detail.
export function RouteTable({
  routes,
  current,
  onOpen,
  admin,
  empty,
}: {
  routes: readonly RouteView[]
  current?: string
  onOpen: (r: RouteView) => void
  admin: boolean
  empty?: ReactNode
}) {
  const columns: Column<RouteView>[] = [
    {
      key: 'state',
      header: 'State',
      className: 'col-state',
      cell: (r) => <StateBadge state={r.state} />,
      sort: (a, b) => compareRouteStates(a.state, b.state) || compareRoutes(a, b),
    },
    { key: 'hostname', header: 'Hostname', lead: true, cell: (r) => <HostCell route={r} admin={admin} />, sort: compareRoutes },
    {
      key: 'owner',
      header: 'Owner',
      className: 'col-owner',
      cell: (r) => <Owner owner={r.owner} guest={r.guest} />,
      sort: (a, b) => compareOwners(a.owner, b.owner) || compareRoutes(a, b),
    },
    { key: 'service', header: 'Service', className: 'mono', cell: (r) => (r.service ? <Untrusted text={r.service} /> : dash) },
    { key: 'level', header: 'Level', className: 'col-level', cell: (r) => <LevelOf route={r} /> },
    {
      key: 'zone',
      header: 'Zone',
      className: 'col-zone',
      cell: (r) => (r.zone ? <Untrusted text={r.zone} /> : dash),
      sort: (a, b) => compareText(a.zone ?? '', b.zone ?? '') || compareRoutes(a, b),
    },
  ]
  return (
    <Table
      label="Routes"
      columns={columns}
      rows={routes}
      rowKey={routeKey}
      defaultSort={{ key: 'hostname', direction: 'ascending' }}
      onActivate={onOpen}
      current={current}
      lines={2}
      empty={empty}
    />
  )
}
