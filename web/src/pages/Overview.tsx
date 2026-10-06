import './overview.css'

import { useMemo, useState } from 'react'

import { useApp } from '../api/store'
import { type EventFilter, EventsTable } from '../app/EventsTable'
import { NoRoutes } from '../app/NoRoutes'
import { navigate, useLocation } from '../app/router'
import { Button } from '../components/Button'
import { Empty } from '../components/Empty'
import { SearchIcon } from '../components/icons'
import { Skeleton } from '../components/Skeleton'
import { ChainList } from '../flow/ChainList'
import { focusRoutes, type Level, mapLevel, mapViewQuery, type MapView, readMapView } from '../flow/collapse'
import { buildModel, firstCycleDone } from '../flow/model'
import { ProblemsCard } from './ProblemsCard'
import { StatTiles } from './StatTiles'

// The kinds of focus that name one thing, as the map and the address bar
// write them; anything else in the search box is words to look for.
const named = /^(hostname|guest|zone|tunnel|route|edge|connector|rogue|path|address|guests|group|more|zones):/

// eventsOf is the filter of the live events for what the map focuses on.
export function eventsOf(focus: string | undefined): EventFilter {
  if (!focus) return {}
  const at = focus.indexOf(':')
  const kind = at < 0 ? '' : focus.slice(0, at)
  const value = focus.slice(at + 1)
  switch (kind) {
    case 'hostname':
      return { route: [value] }
    case 'route':
      return { route: [value.split(' ')[0] ?? value] }
    case 'guest':
      return { guest: [value] }
    case 'tunnel':
    case 'edge':
    case 'connector':
      return { account: [value] }
    case 'zone':
    case 'address':
      return { text: value }
  }
  return named.test(focus) ? {} : { text: focus }
}

function Head() {
  return (
    <div className="page-head">
      <div>
        <h1>Overview</h1>
        <p className="page-description">What pco publishes, how traffic reaches each guest, and what needs you.</p>
      </div>
    </div>
  )
}

// Overview is the first page: the figures, the problems, every hostname's
// chain from the edge to its guest, and the live events. Its focus and what
// is opened are in the address, so a view can be shared.
export function Overview() {
  const st = useApp((s) => s.state)
  const traffic = useApp((s) => s.traffic)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const gateTag = useApp((s) => s.settings?.settings.gateTag)
  const location = useLocation()
  const url = new URL(location, 'https://page.invalid')
  const view = readMapView(url.search)
  const [live, setLive] = useState(true)

  const model = useMemo(() => (st ? buildModel(st, traffic) : undefined), [st, traffic])
  const only = useMemo(() => (model && view.focus ? focusRoutes(model, view.focus) : undefined), [model, view.focus])
  // The level of the map is held from one state to the next, so that near a
  // limit "Problems first" does not change its default every cycle.
  const [level, setLevel] = useState<Level>()
  const now = model ? mapLevel(model, level) : undefined
  if (now !== undefined && now !== level) setLevel(now)

  const setView = (next: MapView) => navigate(`${url.pathname}${mapViewQuery(url.search, next)}${url.hash}`, true)

  if (!st || !model) {
    return (
      <>
        <Head />
        <Skeleton lines={4} label="Loading the state" />
      </>
    )
  }

  const problems = view.problems ?? now === 'collapsed'
  const hostnames = st.routes.length + st.unapproved.reduce((n, g) => n + g.hostnames.length, 0)
  let region
  if (!firstCycleDone(st)) region = <Empty title="Waiting for the first cycle" />
  else if (hostnames === 0) region = <NoRoutes state={st} gateTag={gateTag} />
  else region = <ChainList state={st} traffic={traffic} only={only} problemsFirst={problems} />

  return (
    <>
      <Head />
      <StatTiles state={st} traffic={traffic} nodeZone={nodeZone} />
      <ProblemsCard state={st} />
      <section className="card flow" id="flow" aria-labelledby="flow-head">
        <div className="card-head">
          <h2 id="flow-head">Flow</h2>
          <span className="flow-bands muted">hostname → edge → connector → path → target</span>
          {hostnames > 0 && (
            <div className="flow-tools">
              <label className="flow-search">
                <SearchIcon />
                <span className="sr-only">Focus a hostname, guest, zone or tunnel</span>
                <input
                  type="search"
                  placeholder="Focus a hostname or guest"
                  value={view.focus ?? ''}
                  onChange={(e) => setView({ ...view, focus: e.currentTarget.value || undefined })}
                />
              </label>
              <label className="flow-toggle">
                <input type="checkbox" checked={problems} onChange={(e) => setView({ ...view, problems: e.currentTarget.checked })} /> Problems first
              </label>
            </div>
          )}
        </div>
        {region}
      </section>
      <section className="card" aria-labelledby="events-head">
        <div className="card-head">
          <h2 id="events-head">Live events</h2>
          <span className="spacer" />
          <Button small aria-pressed={!live} onClick={() => setLive(!live)}>
            {live ? 'Pause' : 'Go live'}
          </Button>
        </div>
        <EventsTable filter={eventsOf(view.focus)} live={live} rows={8} />
      </section>
    </>
  )
}
