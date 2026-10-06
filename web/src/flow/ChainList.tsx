import './chains.css'

import { type KeyboardEvent, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react'

import type { State, TrafficView } from '../api/types.gen'
import { Badge } from '../components/Badge'
import { WaitIcon } from '../components/icons'
import { StateBadge, StatusBadge } from '../components/StateBadge'
import { Stepper } from '../components/Stepper'
import { Untrusted } from '../components/Untrusted'
import { marked } from '../text/chars'
import { buildChains, type Chain, trunksOf } from './model'

// The height of a row that is not open, and the margin of rows kept in the
// DOM above and below those in view (chains.css draws them so).
export const chainRow = 40
const overscan = 600

// An open row's height before it was measured: its head, a line per step
// and the figure of its target.
const estimate = (c: Chain) => chainRow + 12 + c.steps.length * 30 + (c.figure ? 26 : 0)

const isProblem = (c: Chain) => c.state !== 'active'

function figureText(f: NonNullable<Chain['figure']>): string {
  const shared = f.shared > 1 ? `, shared by ${f.shared} routes` : ''
  return `${f.rate.toFixed(1)} new connections per second from the connector to ${f.target}${shared}${f.stale ? ' (no new samples)' : ''}`
}

function Owner({ c }: { c: Chain }) {
  return (
    <span className="chain-owner">
      {c.guest && (
        <>
          <Untrusted text={c.guest} max={24} />{' '}
        </>
      )}
      <span className="mono">{c.owner}</span>
    </span>
  )
}

function StateOf({ c }: { c: Chain }) {
  if (c.state === 'unapproved') {
    return (
      <StatusBadge tone="info" icon={<WaitIcon />}>
        waits for approval
      </StatusBadge>
    )
  }
  return <StateBadge state={c.state} />
}

// TrunkStrip is the figures of each tunnel's trunk, on top of the list.
function TrunkStrip({ state, traffic }: { state: State; traffic?: TrafficView }) {
  const trunks = trunksOf(state, traffic)
  if (trunks.length === 0) return null
  return (
    <ul className="trunk-strip" aria-label="Traffic between the edge and the connectors">
      {trunks.map((t) => (
        <li key={t.account}>
          <span className="trunk-name">
            <Untrusted text={t.label} />
          </span>{' '}
          {t.rps === undefined ? (
            <span className="muted">no figures yet</span>
          ) : (
            <span className={t.stale ? 'num muted' : 'num traffic-figure'}>
              {t.rps.toFixed(1)} req/s · {(t.errors ?? 0).toFixed(1)} errors/s{t.stale ? ' (no new samples)' : ''}
            </span>
          )}{' '}
          <span className="muted">{t.ready ? 'connector ready' : 'connector not ready'}</span>
        </li>
      ))}
    </ul>
  )
}

export interface ChainListProps {
  state: State
  traffic?: TrafficView
  // Only these routes, by routeKey: a focus, or a group the map opened.
  only?: ReadonlySet<string>
  problemsFirst?: boolean
  // The most the list takes on the page, in pixels; it scrolls past that.
  height?: number
  label?: string
}

// ChainList is every hostname with its state, one row each; a row opens its
// chain as a stepper (zone, edge, connector, path, target) with the
// figures of its target. It is the map's text alternative and the only view
// on a phone, and it has only the rows in view in the DOM.
export function ChainList({ state, traffic, only, problemsFirst, height = 560, label = 'Hostnames and their chains' }: ChainListProps) {
  const all = useMemo(() => buildChains(state, traffic), [state, traffic])
  const chains = useMemo(() => {
    const shown = only ? all.filter((c) => only.has(c.key)) : all
    return problemsFirst ? [...shown].sort((a, b) => Number(isProblem(b)) - Number(isProblem(a))) : shown
  }, [all, only, problemsFirst])
  const id = useId()
  const scroller = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set())
  const [measured, setMeasured] = useState<ReadonlyMap<string, number>>(new Map())
  const [scrollTop, setScrollTop] = useState(0)
  const [viewport, setViewport] = useState(height)
  const [active, setActive] = useState<string>()
  const focusAfter = useRef<string | undefined>(undefined)

  useEffect(() => {
    const el = scroller.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(() => setViewport(el.clientHeight || height))
    observer.observe(el)
    return () => observer.disconnect()
  }, [height])

  const heightOf = (c: Chain) => (open.has(c.key) ? (measured.get(c.key) ?? estimate(c)) : chainRow)
  const offsets: number[] = [0]
  for (const c of chains) offsets.push((offsets.at(-1) ?? 0) + heightOf(c))
  const total = offsets.at(-1) ?? 0
  let start = 0
  while (start < chains.length && (offsets[start + 1] ?? 0) < scrollTop - overscan) start++
  let end = start
  while (end < chains.length && (offsets[end] ?? 0) < scrollTop + viewport + overscan) end++

  const activeIndex = active === undefined ? -1 : chains.findIndex((c) => c.key === active)
  const tabStop = activeIndex >= start && activeIndex < end ? activeIndex : start

  // An open row is measured as drawn, so the rows under it sit where they
  // are.
  const rows = useRef(new Map<string, HTMLLIElement>())
  const sizes = useRef<ResizeObserver | null>(null)
  useEffect(() => {
    if (typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver((entries) =>
      setMeasured((was) => {
        let next: Map<string, number> | undefined
        for (const e of entries) {
          const key = e.target instanceof HTMLElement ? e.target.dataset.key : undefined
          const h = e.target.getBoundingClientRect().height
          if (key === undefined || h <= 0 || Math.abs((was.get(key) ?? 0) - h) <= 1) continue
          next ??= new Map(was)
          next.set(key, h)
        }
        return next ?? was
      }),
    )
    sizes.current = observer
    return () => {
      observer.disconnect()
      sizes.current = null
    }
  }, [])

  useLayoutEffect(() => {
    const key = focusAfter.current
    if (key === undefined) return
    const el = rows.current.get(key)?.querySelector<HTMLButtonElement>('.chain-head')
    if (!el) return
    focusAfter.current = undefined
    el.focus()
  })

  const toggle = (key: string) => {
    const next = new Set(open)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    setOpen(next)
  }

  const moveTo = (index: number) => {
    const to = Math.min(Math.max(index, 0), chains.length - 1)
    const c = chains[to]
    const el = scroller.current
    if (!c || !el) return
    setActive(c.key)
    const top = offsets[to] ?? 0
    const bottom = offsets[to + 1] ?? top
    let next = scrollTop
    if (top < next) next = top
    else if (bottom > next + viewport) next = bottom - viewport
    if (next !== scrollTop) {
      el.scrollTop = next
      setScrollTop(next)
    }
    const shown = rows.current.get(c.key)?.querySelector<HTMLButtonElement>('.chain-head')
    if (shown) shown.focus()
    else focusAfter.current = c.key
  }

  const keyDown = (e: KeyboardEvent<HTMLButtonElement>, index: number) => {
    const page = Math.max(Math.floor(viewport / chainRow) - 1, 1)
    const moves: Record<string, number> = {
      ArrowDown: index + 1,
      ArrowUp: index - 1,
      PageDown: index + page,
      PageUp: index - page,
      Home: 0,
      End: chains.length - 1,
    }
    if (!Object.hasOwn(moves, e.key)) return
    e.preventDefault()
    moveTo(moves[e.key] ?? index)
  }

  return (
    <div className="chain-list">
      <TrunkStrip state={state} traffic={traffic} />
      {chains.length === 0 ? (
        <p className="chains-empty muted">{only ? 'No hostname matches.' : 'No hostnames.'}</p>
      ) : (
        <div ref={scroller} className="chains-scroll" style={{ maxHeight: height }} onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}>
          <ul className="chains" aria-label={label} style={{ paddingTop: offsets[start] ?? 0, paddingBottom: total - (offsets[end] ?? total) }}>
            {chains.slice(start, end).map((c, at) => {
              const index = start + at
              const expanded = open.has(c.key)
              const body = `${id}-${index}`
              return (
                <li
                  key={c.key}
                  aria-setsize={chains.length}
                  aria-posinset={index + 1}
                  className={expanded ? 'chain chain-open' : 'chain'}
                  data-key={c.key}
                  ref={(el) => {
                    if (!el) return
                    rows.current.set(c.key, el)
                    if (expanded) sizes.current?.observe(el)
                    return () => {
                      rows.current.delete(c.key)
                      sizes.current?.unobserve(el)
                    }
                  }}
                >
                  <button
                    type="button"
                    className="chain-head"
                    aria-expanded={expanded}
                    aria-controls={expanded ? body : undefined}
                    tabIndex={index === tabStop ? 0 : -1}
                    onFocus={() => setActive(c.key)}
                    onClick={() => {
                      setActive(c.key)
                      toggle(c.key)
                    }}
                    onKeyDown={(e) => keyDown(e, index)}
                  >
                    <StateOf c={c} />
                    <span className="chain-host">
                      <Untrusted text={c.hostname} hostname />
                    </span>
                    {c.tags
                      .filter((t) => t !== 'waits for approval')
                      .map((t) => (
                        <Badge key={t} tone={t === 'DNS' ? 'fail' : 'idle'}>
                          {t}
                        </Badge>
                      ))}
                    <Owner c={c} />
                  </button>
                  {expanded && (
                    <div id={body} className="chain-body">
                      <Stepper
                        label={`The chain of ${marked(c.hostname)}`}
                        steps={c.steps.map((s) => ({ key: s.key, name: s.name, level: s.level, word: <Untrusted text={s.text} /> }))}
                      />
                      {c.figure && <p className="chain-figure num">{figureText(c.figure)}</p>}
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        </div>
      )}
    </div>
  )
}
