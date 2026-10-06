import './routes.css'

import { useId, useState, useSyncExternalStore } from 'react'

import { api } from '../../api/client'
import { useApp } from '../../api/store'
import type { RouteView, State } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { NoRoutes } from '../../app/NoRoutes'
import { navigate, type View } from '../../app/router'
import { Button } from '../../components/Button'
import { Drawer } from '../../components/Drawer'
import { RefreshIcon, SearchIcon } from '../../components/icons'
import { Skeleton } from '../../components/Skeleton'
import { StateBadge } from '../../components/StateBadge'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { routeStateOrder } from '../../gen/words.gen'
import { marked } from '../../text/chars'
import { ManualRoutePage } from './ManualRouteForm'
import { errorMessage, NoteLine, PageHead, readerReason, RoutesNav, useAdmin } from './parts'
import { PlanPage } from './PlanPage'
import { RouteDetail } from './RouteDetail'
import { compareRoutes, holderOf, matchesRoute, noFilter, type RouteFilter, routeKey, routeLink, stateCounts, unset } from './routes'
import { RouteTable } from './RouteTable'

// A phone shows the detail of a route as a page of its own, a desktop beside
// the table.
const phone = '(max-width: 719px)'

function followPhone(changed: () => void): () => void {
  const query = window.matchMedia(phone)
  query.addEventListener('change', changed)
  return () => query.removeEventListener('change', changed)
}

function usePhone(): boolean {
  return useSyncExternalStore(followPhone, () => window.matchMedia(phone).matches)
}

// HoldNote is the sentence pco routes prints over routes it did not see in
// the last cycle, or that no cycle has run yet.
function HoldNote({ at, hold, complete }: { at?: string; hold?: string; complete: boolean }) {
  if (unset(at)) {
    return (
      <NoteLine tone="info" status>
        Waiting for the first cycle.
      </NoteLine>
    )
  }
  if (hold) {
    return (
      <NoteLine>
        The last cycle held (<Untrusted text={hold} />
        ): these are the routes of an earlier cycle.
      </NoteLine>
    )
  }
  if (!complete) {
    return (
      <NoteLine>The inventory is incomplete: these are the routes of an earlier cycle.</NoteLine>
    )
  }
  return null
}

function Filters({ routes, filter, onChange, shown }: { routes: readonly RouteView[]; filter: RouteFilter; onChange: (f: RouteFilter) => void; shown: number }) {
  const searchId = useId()
  const zoneId = useId()
  const zones = [...new Set(routes.map((r) => r.zone ?? '').filter(Boolean))].sort()
  const toggle = (state: string) =>
    onChange({ ...filter, states: filter.states.includes(state) ? filter.states.filter((s) => s !== state) : [...filter.states, state] })
  const filtered = filter.states.length > 0 || filter.zone !== '' || filter.text.trim() !== ''
  return (
    <div className="route-filters">
      <div className="filter-chips" role="group" aria-label="States">
        {stateCounts(routes, routeStateOrder).map(([state, n]) => (
          <button key={state} type="button" className="filter-chip" aria-pressed={filter.states.includes(state)} onClick={() => toggle(state)}>
            <StateBadge state={state} /> <span className="num filter-count">{n}</span>
          </button>
        ))}
      </div>
      <div className="filter-fields">
        <label htmlFor={zoneId} className="sr-only">
          Zone
        </label>
        <select id={zoneId} className="filter-zone" value={filter.zone} onChange={(e) => onChange({ ...filter, zone: e.target.value })}>
          <option value="">All zones</option>
          {zones.map((z) => (
            <option key={z} value={z}>
              {marked(z)}
            </option>
          ))}
        </select>
        <span className="filter-search">
          <SearchIcon />
          <label htmlFor={searchId} className="sr-only">
            Search the routes
          </label>
          <input
            id={searchId}
            type="search"
            placeholder="Hostname, owner, guest or service"
            value={filter.text}
            onChange={(e) => onChange({ ...filter, text: e.target.value })}
          />
        </span>
        <span className="muted num" role="status">
          {shown === routes.length ? `${routes.length} routes` : `${shown} of ${routes.length} routes`}
        </span>
        {filtered && (
          <button type="button" className="linkbtn" onClick={() => onChange(noFilter)}>
            Clear the filter
          </button>
        )}
      </div>
    </div>
  )
}

// RoutesPage is /routes and /routes/:hostname?owner=: every route with its
// state, filtered and searched here, and the detail of the one selected,
// whose address is the page's.
export function RoutesPage({ view }: { view: Extract<View, { name: 'routes' | 'route' }> }) {
  const st = useApp((s) => s.state)
  const gateTag = useApp((s) => s.settings?.settings.gateTag)
  const admin = useAdmin()
  const toast = useToast()
  const onPhone = usePhone()
  const [filter, setFilter] = useState<RouteFilter>(noFilter)
  const [syncing, setSyncing] = useState(false)

  const sync = async () => {
    setSyncing(true)
    try {
      await api('POST', '/api/v1/sync', {})
      toast('A cycle was requested.', 'ok')
    } catch (e) {
      toast(<Untrusted text={errorMessage(e)} />, 'fail')
    } finally {
      setSyncing(false)
    }
  }

  const selected = view.name === 'route' ? view : undefined
  const head = (
    <PageHead
      title="Routes"
      description="Every hostname the guests and the manual routes ask for, and what pco made of it."
      actions={
        <>
          <Button icon={<RefreshIcon />} onClick={() => void sync()} disabledReason={!admin ? readerReason : syncing ? 'a cycle was just asked for' : undefined}>
            Sync now
          </Button>
          {admin ? (
            <Link to="/routes/manual/new" className="btn btn-primary">
              New manual route
            </Link>
          ) : (
            <Button variant="primary" disabledReason={readerReason}>
              New manual route
            </Button>
          )}
        </>
      }
    />
  )

  if (selected && onPhone) {
    return (
      <>
        <p>
          <Link to="/routes">All routes</Link>
        </p>
        <RouteDetail key={routeKey({ hostname: selected.hostname, owner: selected.owner ?? '' })} hostname={selected.hostname} owner={selected.owner} variant="page" />
      </>
    )
  }

  const drawer = (
    <Drawer open={selected !== undefined} onClose={() => navigate('/routes')} title={selected ? <Untrusted text={selected.hostname} hostname /> : ''}>
      {selected && (
        <RouteDetail key={routeKey({ hostname: selected.hostname, owner: selected.owner ?? '' })} hostname={selected.hostname} owner={selected.owner} variant="drawer" />
      )}
    </Drawer>
  )

  return (
    <>
      {head}
      <RoutesNav current="routes" />
      {st ? <RouteList st={st} gateTag={gateTag} admin={admin} selected={selected} filter={filter} onFilter={setFilter} /> : <Skeleton lines={6} label="Loading the routes" />}
      {drawer}
    </>
  )
}

// RouteList is the table of the routes as the filter leaves them, or why
// there is none.
function RouteList({
  st,
  gateTag,
  admin,
  selected,
  filter,
  onFilter,
}: {
  st: State
  gateTag?: string
  admin: boolean
  selected?: { hostname: string; owner?: string }
  filter: RouteFilter
  onFilter: (f: RouteFilter) => void
}) {
  const shown = st.routes.filter((r) => matchesRoute(r, filter)).sort(compareRoutes)
  const open = selected && (selected.owner ? { hostname: selected.hostname, owner: selected.owner } : holderOf(st.routes, selected.hostname))
  const current = open && routeKey(open)
  return (
    <>
      {/* before the first cycle there are no routes, and NoRoutes says why */}
      {(st.routes.length > 0 || !unset(st.at)) && <HoldNote at={st.at} hold={st.hold} complete={st.complete} />}
      {st.routes.length === 0 ? (
        <NoRoutes state={st} gateTag={gateTag} />
      ) : (
        <>
          <Filters routes={st.routes} filter={filter} onChange={onFilter} shown={shown.length} />
          <RouteTable
            routes={shown}
            current={current}
            admin={admin}
            onOpen={(r) => navigate(routeLink(r.hostname, r.owner))}
            empty={
              <>
                No route matches the filter.{' '}
                <button type="button" className="linkbtn" onClick={() => onFilter(noFilter)}>
                  Clear the filter
                </button>
              </>
            }
          />
        </>
      )}
    </>
  )
}

// RoutesSection is every view under /routes. The list and a route's detail
// are one page, so that the filter stays as it is while routes are opened.
export function RoutesSection({ view }: { view: Extract<View, { name: 'routes' | 'route' | 'plan' | 'manual-new' | 'manual' }> }) {
  switch (view.name) {
    case 'plan':
      return <PlanPage />
    case 'manual-new':
      return <ManualRoutePage key="new" />
    case 'manual':
      return <ManualRoutePage key={view.id} id={view.id} />
  }
  return <RoutesPage view={view} />
}
