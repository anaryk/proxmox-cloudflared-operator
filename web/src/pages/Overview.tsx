import './overview.css'

import { lazy, type MouseEvent, Suspense, useMemo, useRef, useState, useSyncExternalStore } from 'react'

import { useApp } from '../api/store'
import { type EventFilter, EventsTable } from '../app/EventsTable'
import { NoRoutes } from '../app/NoRoutes'
import { navigate, useLocation } from '../app/router'
import { Button, IconButton } from '../components/Button'
import { Empty } from '../components/Empty'
import { ListIcon, PauseIcon, PlayIcon, SearchIcon } from '../components/icons'
import { Skeleton } from '../components/Skeleton'
import { ChainList } from '../flow/ChainList'
import { collapse, expandAll, focusRoutes, type Level, mapLevel, mapViewQuery, type MapView, readMapView } from '../flow/collapse'
import { FlowDrawer } from '../flow/FlowDrawer'
import { buildModel, firstCycleDone } from '../flow/model'
import type { ZoomAsk } from '../flow/types'
import { mapWords } from '../text/flow'
import { ProblemsCard } from './ProblemsCard'
import { StatTiles } from './StatTiles'

// The map is drawn by code of its own, loaded when it is first shown.
const FlowPanel = lazy(() => import('../flow/FlowMap').then((m) => ({ default: m.FlowPanel })))

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

// Below this width the map gives way to the chain list.
const phone = '(max-width: 719px)'

function followPhone(changed: () => void): () => void {
  const query = window.matchMedia(phone)
  query.addEventListener('change', changed)
  return () => query.removeEventListener('change', changed)
}

function usePhone(): boolean {
  return useSyncExternalStore(followPhone, () => window.matchMedia(phone).matches)
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
// chain from the edge to its guest as a map or a list, and the live events.
// Its focus, what is opened and the list view are in the address, so a view
// can be shared.
export function Overview() {
  const st = useApp((s) => s.state)
  const traffic = useApp((s) => s.traffic)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const gateTag = useApp((s) => s.settings?.settings.gateTag)
  const location = useLocation()
  const url = new URL(location, 'https://page.invalid')
  const search = url.search
  const view = useMemo(() => readMapView(search), [search])
  const listAsked = new URLSearchParams(search).get('list') === '1'
  const phoneNow = usePhone()
  const [live, setLive] = useState(true)
  const [selected, setSelected] = useState<string>()
  const [paused, setPaused] = useState(false)
  const [zoom, setZoom] = useState<ZoomAsk>()
  const after = useRef<HTMLSpanElement>(null)

  const model = useMemo(() => (st ? buildModel(st, traffic) : undefined), [st, traffic])
  const only = useMemo(() => (model && view.focus ? focusRoutes(model, view.focus) : undefined), [model, view.focus])
  // The level of the map is held from one state to the next, so that a map
  // near a threshold does not switch on every cycle, nor the default of
  // "Problems first" with it.
  const [level, setLevel] = useState<Level>()
  const levelNow = model ? mapLevel(model, level) : undefined
  if (levelNow !== undefined && levelNow !== level) setLevel(levelNow)
  const problems = view.problems ?? levelNow === 'collapsed'
  const shaped = useMemo(
    () => (model ? collapse(model, { focus: view.focus, expanded: view.expanded, previousLevel: level, problems }).model : undefined),
    [model, view, level, problems],
  )

  const setView = (next: MapView) => navigate(`${url.pathname}${mapViewQuery(search, next)}${url.hash}`, true)
  const setList = (on: boolean) => {
    const q = new URLSearchParams(search)
    if (on) q.set('list', '1')
    else q.delete('list')
    const s = q.toString()
    navigate(`${url.pathname}${s ? `?${s}` : ''}${url.hash}`, true)
  }
  const expand = (id: string) => setView({ ...view, expanded: new Set([...view.expanded, id]) })
  const allOpen = view.expanded.has(expandAll)
  const skip = (e: MouseEvent<HTMLAnchorElement>) => {
    e.preventDefault()
    after.current?.focus()
  }

  if (!st || !model) {
    return (
      <>
        <Head />
        <Skeleton lines={4} label="Loading the state" />
      </>
    )
  }

  const hostnames = st.routes.length + st.unapproved.reduce((n, g) => n + g.hostnames.length, 0)
  const drawn = firstCycleDone(st) && hostnames > 0
  const listView = phoneNow || listAsked
  let region
  if (!firstCycleDone(st)) region = <Empty title="Waiting for the first cycle" />
  else if (hostnames === 0) region = <NoRoutes state={st} gateTag={gateTag} />
  else if (listView) region = <ChainList state={st} traffic={traffic} only={only} problemsFirst={problems} />
  else {
    region = (
      <>
        <a className="flow-skip" href="#flow-end" onClick={skip}>
          {mapWords.skip}
        </a>
        <Suspense fallback={<Skeleton lines={6} label={mapWords.loading} />}>
          <FlowPanel model={shaped ?? model} selected={selected} onSelect={setSelected} onExpand={expand} paused={paused} zoom={zoom} />
        </Suspense>
        <span id="flow-end" ref={after} tabIndex={-1} className="sr-only">
          End of the map
        </span>
      </>
    )
  }

  return (
    <>
      <Head />
      <StatTiles state={st} traffic={traffic} nodeZone={nodeZone} />
      <ProblemsCard state={st} />
      <section className="card flow" id="flow" aria-labelledby="flow-head">
        <div className="card-head">
          <h2 id="flow-head">Flow</h2>
          <span className="flow-bands muted">{mapWords.bands}</span>
          {drawn && (
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
              {!listView && (
                <>
                  <Button
                    small
                    aria-pressed={allOpen}
                    onClick={() => setView({ ...view, expanded: new Set(allOpen ? [...view.expanded].filter((id) => id !== expandAll) : [...view.expanded, expandAll]) })}
                  >
                    {mapWords.expandAll}
                  </Button>
                  <Button small icon={paused ? <PlayIcon /> : <PauseIcon />} aria-pressed={paused} onClick={() => setPaused(!paused)}>
                    {paused ? mapWords.resume : mapWords.pause}
                  </Button>
                  <span className="flow-zoom">
                    <IconButton label={mapWords.zoomOut} icon={<span aria-hidden="true">−</span>} onClick={() => setZoom({ to: 'out', n: (zoom?.n ?? 0) + 1 })} />
                    <Button small onClick={() => setZoom({ to: 'fit', n: (zoom?.n ?? 0) + 1 })}>
                      {mapWords.fit}
                    </Button>
                    <IconButton label={mapWords.zoomIn} icon={<span aria-hidden="true">+</span>} onClick={() => setZoom({ to: 'in', n: (zoom?.n ?? 0) + 1 })} />
                  </span>
                </>
              )}
              {!phoneNow && (
                <Button small icon={<ListIcon />} aria-pressed={listAsked} onClick={() => setList(!listAsked)}>
                  {mapWords.listView}
                </Button>
              )}
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
      {!listView && <FlowDrawer model={shaped} selected={selected} onClose={() => setSelected(undefined)} />}
    </>
  )
}
