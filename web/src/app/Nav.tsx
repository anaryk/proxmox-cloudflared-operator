import type { ComponentType, ReactNode } from 'react'

import { useApp } from '../api/store'
import type { State } from '../api/types.gen'
import { IconButton } from '../components/Button'
import {
  DoctorIcon,
  EventsIcon,
  GlobeIcon,
  GuestIcon,
  type IconProps,
  KeyIcon,
  ListIcon,
  NetworkIcon,
  OverviewIcon,
  RouteIcon,
  SettingsIcon,
  TunnelIcon,
} from '../components/icons'
import { credentialState } from '../text/words'
import { Link } from './Link'
import { type Section, sectionOf, useView } from './router'

// What needs a person, by item of the navigation (spec-ui 3.1). All of it is
// read from the state: the page never fetches the claims or runs the doctor
// for a counter.
export interface Counters {
  routes: number // routes not active
  guests: number // guests waiting for approval, routes in conflict
  credentials: number // credentials not usable
  zones: number // zones frozen
  tunnels: number // connectors pco does not run
  edge: number // the three above
  doctor?: number // failures of this browser's last run, for admins
}

export function countersOf(st: State | undefined, role: string | undefined, doctorLast?: { fail: number }): Counters {
  const routes = st?.routes ?? []
  const credentials = (st?.credentials ?? []).filter((c) => credentialState(c) !== 'usable').length
  const zones = (st?.zones ?? []).filter((z) => z.state === 'frozen').length
  const tunnels = (st?.rogueConnectors ?? []).length
  return {
    routes: routes.filter((r) => r.state !== 'active').length,
    guests: (st?.unapproved ?? []).length + routes.filter((r) => r.state === 'conflict').length,
    credentials,
    zones,
    tunnels,
    edge: credentials + zones + tunnels,
    doctor: role === 'admin' && doctorLast ? doctorLast.fail : undefined,
  }
}

interface Item {
  section: Section
  to: string
  label: string
  Icon: ComponentType<IconProps>
  count?: number
  why?: string
}

function NavLink({ item, current }: { item: Item; current: boolean }) {
  return (
    <Link to={item.to} className="navlink" aria-current={current ? 'page' : undefined} title={item.label}>
      <item.Icon />
      <span className="nav-label">{item.label}</span>
      {item.count !== undefined && item.count > 0 && (
        <span className="count" title={item.why}>
          {item.count}
          <span className="sr-only"> {item.why}</span>
        </span>
      )}
    </Link>
  )
}

function Group({ label, count, children }: { label?: string; count?: number; children: ReactNode }) {
  if (!label) return <>{children}</>
  return (
    <div className="nav-group" role="group" aria-label={label}>
      <div className="grp" aria-hidden="true">
        {label}
        {count !== undefined && count > 0 && <span className="grp-count">{count}</span>}
      </div>
      {children}
    </div>
  )
}

// Nav is the navigation on the left: no group, Edge and Operate, as the
// mockup has them. There is no item for the first-run setup: it shows itself
// while there is no credential and stays reachable from Settings.
export function Nav({ open, collapsed, onCollapse, onAbout }: { open: boolean; collapsed: boolean; onCollapse: () => void; onAbout: () => void }) {
  const st = useApp((s) => s.state)
  const role = useApp((s) => s.session?.role)
  const doctorLast = useApp((s) => s.doctorLast)
  const version = useApp((s) => s.session?.version)
  const section = sectionOf(useView())
  const c = countersOf(st, role, doctorLast)

  const groups: { label?: string; count?: number; items: Item[] }[] = [
    {
      items: [
        { section: 'overview', to: '/', label: 'Overview', Icon: OverviewIcon },
        { section: 'routes', to: '/routes', label: 'Routes', Icon: RouteIcon, count: c.routes, why: 'routes that are not active' },
        { section: 'guests', to: '/guests', label: 'Guests', Icon: GuestIcon, count: c.guests, why: 'guests waiting for approval and routes in conflict' },
        { section: 'networks', to: '/networks', label: 'Networks', Icon: NetworkIcon },
      ],
    },
    {
      label: 'Edge',
      count: c.edge,
      items: [
        { section: 'credentials', to: '/edge/credentials', label: 'Credentials', Icon: KeyIcon, count: c.credentials, why: 'credentials that are not usable' },
        { section: 'zones', to: '/edge/zones', label: 'Zones', Icon: GlobeIcon, count: c.zones, why: 'zones that are frozen' },
        { section: 'tunnels', to: '/edge/tunnels', label: 'Tunnels', Icon: TunnelIcon, count: c.tunnels, why: 'connectors pco does not run' },
      ],
    },
    {
      label: 'Operate',
      items: [
        { section: 'events', to: '/events', label: 'Events', Icon: EventsIcon },
        { section: 'doctor', to: '/doctor', label: 'Doctor', Icon: DoctorIcon, count: c.doctor, why: 'failures of the last run in this browser' },
        { section: 'settings', to: '/settings', label: 'Settings', Icon: SettingsIcon },
      ],
    },
  ]

  return (
    <nav id="nav" className={['nav', open && 'nav-open', collapsed && 'nav-collapsed'].filter(Boolean).join(' ')} aria-label="Main">
      {groups.map((g) => (
        <Group key={g.label ?? 'main'} label={g.label} count={g.count}>
          {g.items.map((item) => (
            <NavLink key={item.section} item={item} current={item.section === section} />
          ))}
        </Group>
      ))}
      <div className="nav-foot">
        <span className="nav-label">
          pco {version}{' '}
          <button type="button" className="linkbtn" onClick={onAbout}>
            About
          </button>
        </span>
        <IconButton label={collapsed ? 'Show the names' : 'Show icons only'} icon={<ListIcon />} aria-pressed={collapsed} onClick={onCollapse} />
      </div>
    </nav>
  )
}
