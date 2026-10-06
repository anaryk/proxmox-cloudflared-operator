import './flow.css'

import { select } from 'd3-selection'
import { type D3ZoomEvent, zoom, zoomIdentity, type ZoomBehavior, type ZoomTransform } from 'd3-zoom'
import { type ComponentType, type JSX, type ReactNode, useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'

import { useApp } from '../api/store'
import { Untrusted } from '../components/Untrusted'
import { edgeTip, mapWords, noDataSince, type PathFigure, pathFigure, portNote, rotateWords, trunkFigure } from '../text/flow'
import type { CommandWords } from '../text/words'
import { pointAt } from './edges/path'
import { StateEdge } from './edges/StateEdge'
import { TargetEdge } from './edges/TargetEdge'
import { Trunk } from './edges/Trunk'
import { buildNav, type Nav, rowItemId, rowRoutes, step } from './keys'
import { layout as layOut } from './layout'
import { Legend } from './Legend'
import { MiniMap } from './MiniMap'
import { createMotion, dotCap } from './motion'
import { ConnectorNode } from './nodes/ConnectorNode'
import { EdgeNode } from './nodes/EdgeNode'
import type { CardProps } from './nodes/parts'
import { PathNode } from './nodes/PathNode'
import { RogueNode } from './nodes/RogueNode'
import { TargetCard } from './nodes/TargetCard'
import { ZoneCard } from './nodes/ZoneCard'
import { changesOf, extrasOf, keepSame, restartable } from './panel'
import type { Box, FlowEdge, FlowMapProps, FlowNode, Layout, MapExtras, Model, Motion, MotionEdge, ZoomAsk } from './types'

// The map is SVG for its lines, HTML for its cards, so that what guests and
// Cloudflare wrote is shown as isolated text, and a second SVG over both for
// the dots, which then move without the cards being painted again. d3-zoom
// pans and zooms the three together.

const maxZoom = 4
const zoomStep = 1.4
const margin = 24

// The map's type is at least 12 px; the fit shrinks it to 11 px and no
// further: a narrower frame scrolls sideways instead.
export const minFit = 11 / 12

// The mini map shows on a wide screen once the map has more cards than a
// screen holds at a glance.
const miniCards = 40
const wideQuery = '(min-width: 1200px)'

function followWide(changed: () => void): () => void {
  if (typeof window.matchMedia !== 'function') return () => undefined
  const query = window.matchMedia(wideQuery)
  query.addEventListener('change', changed)
  return () => query.removeEventListener('change', changed)
}

const wideNow = () => typeof window.matchMedia === 'function' && window.matchMedia(wideQuery).matches

// gestureFilter says which events pan and zoom the map: a drag of the
// primary button, the wheel with Ctrl (a pinch of a touchpad), and two
// fingers. One finger is the page's, which scrolls (the frame allows pan-y),
// and the tooltip and the mini map keep their own presses.
export function gestureFilter(e: Event): boolean {
  if (e.target instanceof Element && e.target.closest('.fm-tip, .fm-mini')) return false
  if (e.type === 'wheel') return !!(e as WheelEvent).ctrlKey
  if (e.type.startsWith('touch')) return ((e as TouchEvent).touches?.length ?? 0) >= 2
  const m = e as MouseEvent
  return !m.ctrlKey && !m.button
}

function cardOf(n: FlowNode): ComponentType<CardProps> {
  switch (n.kind) {
    case 'zone':
      return ZoneCard
    case 'edge':
      return EdgeNode
    case 'connector':
      return ConnectorNode
    case 'rogue':
      return RogueNode
    case 'path':
      return PathNode
    case 'group':
      return n.band === 'hostnames' ? ZoneCard : TargetCard
  }
  return TargetCard
}

// What the pointer or the focus lights: the chains of the routes of what it
// is on, and the rest dimmed. What has no route lights itself, its lines
// and what they join.
interface Lit {
  cards: ReadonlyMap<string, string>
  edges: ReadonlySet<string>
}

function lightUp(model: Model, nav: Nav, hot: string, chain?: string): Lit {
  const item = nav.byId.get(hot)
  const edge = item ? undefined : model.edges.find((e) => e.id === hot)
  const routes = new Set(chain !== undefined && item?.routes.includes(chain) ? [chain] : item ? item.routes : (edge?.routes ?? []))
  const cards = new Map<string, string>()
  const edges = new Set<string>()
  if (routes.size === 0) {
    const self = item?.node.id ?? ''
    for (const e of model.edges) {
      if (e.id !== hot && e.from !== self && e.to !== self) continue
      edges.add(e.id)
      cards.set(e.from, '*')
      cards.set(e.to, '*')
    }
    if (item) cards.set(self, item.row ? rowItemId(item.row) : '*')
    return { cards, edges }
  }
  const touches = (list: readonly string[] | undefined) => (list ?? []).some((k) => routes.has(k))
  for (const e of model.edges) if (touches(e.routes)) edges.add(e.id)
  for (const n of model.nodes) {
    const parts = n.rows ? n.rows.map((r) => [rowItemId(r), rowRoutes(r)] as const) : (n.ports ?? []).map((p) => [p.id, p.routes ?? []] as const)
    if (parts.length > 0) {
      const on = parts.filter(([, r]) => touches(r)).map(([id]) => id)
      if (on.length === parts.length) cards.set(n.id, '*')
      else if (on.length > 0) cards.set(n.id, on.join('\n'))
      else if (touches(n.routes)) cards.set(n.id, '')
    } else if (touches(n.routes)) {
      cards.set(n.id, '*')
    }
  }
  if (item && !cards.has(item.node.id)) cards.set(item.node.id, '*')
  return { cards, edges }
}

const endsAtTarget = (e: FlowEdge, byId: ReadonlyMap<string, FlowNode>) => e.port !== undefined || byId.get(e.to)?.band === 'targets'

function Command({ cmd }: { cmd: CommandWords }) {
  if (cmd.command) return <code className="fm-tip-code">{cmd.command.text}</code>
  return (
    <span className="fm-tip-line">
      {rotateWords.none} <Untrusted text={cmd.refused} />
    </span>
  )
}

// Where the tooltip shows, in the frame: under the item or the pointer, or
// over it near the frame's foot, which would cut it.
interface Tip {
  id: string
  x: number
  y: number
  above?: boolean
}

const tipRoom = 110

// FlowMap draws a view of the model where layout placed it, fitted to its
// width, and gives the motion the lines its dots run on.
export default function FlowMap(props: FlowMapProps): JSX.Element {
  const { model, layout, motion, selected, focus, reducedMotion, extras, stale, changed, zoom: ask } = props
  const instructions = useId()
  const tipId = useId()
  const [frame, setFrame] = useState<HTMLDivElement | null>(null)
  const view = useRef<HTMLDivElement>(null)
  const [size, setSize] = useState({ width: 0, height: 0 })
  const [pointer, setPointer] = useState<string>()
  // The item with the focus, and the chain the keys follow through it.
  const [focused, setFocused] = useState<{ id: string; chain?: string }>()
  const keyed = focused?.id
  const [tip, setTip] = useState<Tip>()
  const transform = useRef<ZoomTransform>(zoomIdentity)
  const behaviour = useRef<ZoomBehavior<HTMLDivElement, unknown> | null>(null)
  const fitted = useRef(true)
  const chain = useRef<string | undefined>(undefined)
  const visible = useRef<ReadonlySet<string> | undefined>(undefined)

  const nav = useMemo(() => buildNav(model, layout), [model, layout])
  const byId = useMemo(() => new Map(model.nodes.map((n) => [n.id, n])), [model])
  const hot = pointer ?? keyed
  const followed = pointer === undefined ? focused?.chain : undefined
  const lit = useMemo(() => (hot ? lightUp(model, nav, hot, followed) : undefined), [model, nav, hot, followed])
  // The map's one tab stop: the item last focused, else the first of the
  // first column.
  const tabStop = focus !== undefined && nav.byId.has(focus) ? focus : [...nav.bands.values()].find((list) => list.length > 0)?.[0]?.id

  // The port each line of a zone card is served on, for its name.
  const portsOf = useMemo(() => {
    const byRoute = new Map<string, string>()
    for (const n of model.nodes) for (const p of n.ports ?? []) for (const k of p.routes ?? []) byRoute.set(k, p.target?.split(':').at(-1) ?? '')
    const out = new Map<string, string>()
    for (const n of model.nodes) {
      if (n.rows) out.set(n.id, n.rows.map((r) => byRoute.get(`${r.hostname} ${r.owner}`) ?? '').join('\n'))
    }
    return out
  }, [model])

  const rings = useMemo(() => {
    const out = new Map<string, string>()
    if (!changed || changed.ids.size === 0) return out
    for (const n of model.nodes) {
      const ids = [n.id, ...(n.rows ?? []).map(rowItemId), ...(n.ports ?? []).map((p) => p.id)].filter((id) => changed.ids.has(id))
      if (ids.length > 0) out.set(n.id, [String(changed.n), ...ids].join('\n'))
    }
    return out
  }, [model, changed])

  // The frame's size, for the fit and the culling.
  useLayoutEffect(() => {
    if (!frame) return
    const measure = () => setSize((was) => (was.width === frame.clientWidth && was.height === frame.clientHeight ? was : { width: frame.clientWidth, height: frame.clientHeight }))
    measure()
    if (typeof ResizeObserver === 'undefined') return
    const watch = new ResizeObserver(measure)
    watch.observe(frame)
    return () => watch.disconnect()
  }, [frame])

  const width = size.width || layout.width
  const height = size.height || layout.height
  const fitScale = Math.max(width / layout.width, minFit)

  // Dots only on the lines in view.
  const cull = useCallback(() => {
    const t = transform.current
    const x0 = -t.x / t.k - margin
    const y0 = -t.y / t.k - margin
    const x1 = x0 + width / t.k + 2 * margin
    const y1 = y0 + height / t.k + 2 * margin
    const ids = new Set<string>()
    for (const [id, { from, to }] of layout.edges) {
      if (Math.max(from.x, to.x) >= x0 && Math.min(from.x, to.x) <= x1 && Math.max(from.y, to.y) >= y0 && Math.min(from.y, to.y) <= y1) ids.add(id)
    }
    const was = visible.current
    if (was && was.size === ids.size && [...ids].every((id) => was.has(id))) return
    visible.current = ids
    motion.setVisible(ids)
  }, [layout, width, height, motion])

  // The rectangle of the mini map follows what the map shows.
  const miniView = useRef<SVGRectElement>(null)
  const viewport = useCallback(
    (t: ZoomTransform) => {
      const rect = miniView.current
      if (!rect) return
      rect.setAttribute('x', String(-t.x / t.k))
      rect.setAttribute('y', String(-t.y / t.k))
      rect.setAttribute('width', String(width / t.k))
      rect.setAttribute('height', String(height / t.k))
    },
    [width, height],
  )

  const onViewport = props.onViewport
  const apply = useCallback(
    (t: ZoomTransform) => {
      transform.current = t
      if (view.current) view.current.style.transform = `translate(${t.x}px, ${t.y}px) scale(${t.k})`
      cull()
      viewport(t)
      onViewport({ x: t.x, y: t.y, zoom: t.k })
    },
    [cull, viewport, onViewport],
  )

  // Pan and zoom: dragging pans, a pinch or the wheel with Ctrl zooms, the
  // wheel alone scrolls the map until it ends and then the page.
  useLayoutEffect(() => {
    if (!frame) return
    const z = zoom<HTMLDivElement, unknown>().filter(gestureFilter).clickDistance(4)
    behaviour.current = z
    const sel = select(frame)
    sel.call(z).on('dblclick.zoom', null)
    const wheel = (e: WheelEvent) => {
      if (e.ctrlKey) return
      const t = transform.current
      const unit = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? frame.clientHeight : 1
      z.translateBy(sel, (-e.deltaX * unit) / t.k, (-e.deltaY * unit) / t.k)
      const now = transform.current
      if (now.x !== t.x || now.y !== t.y) e.preventDefault()
    }
    frame.addEventListener('wheel', wheel, { passive: false })
    return () => {
      frame.removeEventListener('wheel', wheel)
      sel.on('.zoom', null)
      behaviour.current = null
    }
  }, [frame])

  useLayoutEffect(() => {
    const z = behaviour.current
    if (!z || !frame) return
    z.extent([
      [0, 0],
      [width, height],
    ])
      .translateExtent([
        [0, 0],
        [layout.width, layout.height],
      ])
      .scaleExtent([Math.min(fitScale, 1) / 2, maxZoom])
      .on('zoom', (e: D3ZoomEvent<HTMLDivElement, unknown>) => {
        if (e.sourceEvent) fitted.current = false
        apply(e.transform)
      })
  }, [frame, width, height, layout.width, layout.height, fitScale, apply])

  // Fit the width, on the first drawing, when the frame changes width while
  // fitted, and when asked; the top of the view stays where it was.
  const fit = useCallback(() => {
    const z = behaviour.current
    if (!z || !frame) return
    const t = transform.current
    const top = -t.y / t.k
    fitted.current = true
    const sel = select(frame)
    z.transform(sel, zoomIdentity.translate(0, -top * fitScale).scale(fitScale))
    // A transform is taken as it is; a move keeps it within the map.
    z.translateBy(sel, 0, 0)
  }, [frame, fitScale])

  useLayoutEffect(() => {
    if (fitted.current) fit()
  }, [fit])

  const asked = useRef<ZoomAsk | undefined>(undefined)
  useEffect(() => {
    if (!ask || ask === asked.current) return
    asked.current = ask
    const z = behaviour.current
    if (!z || !frame) return
    if (ask.to === 'fit') return fit()
    fitted.current = false
    z.scaleBy(select(frame), ask.to === 'in' ? zoomStep : 1 / zoomStep)
  }, [ask, fit, frame])

  // The lines the dots run on, and those of them in view.
  const layer = useCallback((g: SVGGElement | null) => motion.attach(g), [motion])
  useEffect(() => {
    const lines: MotionEdge[] = []
    for (const edge of model.edges) {
      const ends = layout.edges.get(edge.id)
      if (ends) lines.push({ edge, at: pointAt(ends.from, ends.to) })
    }
    motion.setEdges(lines)
  }, [model.edges, layout, motion])
  useEffect(() => {
    visible.current = undefined
    cull()
  }, [cull])

  // reveal brings an item into view, for the keyboard.
  const reveal = useCallback(
    (b: Box) => {
      const z = behaviour.current
      if (!z || !frame) return
      const t = transform.current
      const left = t.x + b.x * t.k
      const top = t.y + b.y * t.k
      const right = left + b.width * t.k
      const bottom = top + b.height * t.k
      let dx = 0
      let dy = 0
      if (top < margin) dy = margin - top
      else if (bottom > height - margin) dy = Math.max(height - margin - bottom, margin - top)
      if (left < 0) dx = -left
      else if (right > width) dx = Math.max(width - right, -left)
      if (dx !== 0 || dy !== 0) z.translateBy(select(frame), dx / t.k, dy / t.k)
    },
    [frame, width, height],
  )

  const wide = useSyncExternalStore(followWide, wideNow)
  const mini = wide && model.nodes.length > miniCards
  useEffect(() => {
    if (mini) viewport(transform.current)
  }, [mini, viewport])
  const pick = useCallback(
    (x: number, y: number) => {
      const z = behaviour.current
      if (z && frame) z.translateTo(select(frame), x, y)
    },
    [frame],
  )

  // What the lines from the connectors into each path carry.
  const carried = useMemo(() => {
    const into = new Map<string, FlowEdge[]>()
    for (const e of model.edges) {
      if (e.rate === undefined || byId.get(e.to)?.kind !== 'path') continue
      into.set(e.to, [...(into.get(e.to) ?? []), e])
    }
    const out = new Map<string, PathFigure>()
    for (const [id, edges] of into) {
      const fresh = edges.filter((e) => !e.stale)
      const sum = (list: FlowEdge[]) => list.reduce((n, e) => n + (e.rate ?? 0), 0)
      out.set(id, fresh.length > 0 ? { rate: sum(fresh) } : { rate: sum(edges), stale: true })
    }
    return out
  }, [model, byId])

  const tipAt = useCallback(
    (id: string): Tip | undefined => {
      const item = nav.byId.get(id)
      if (!item) return undefined
      const t = transform.current
      const below = t.y + (item.box.y + item.box.height) * t.k
      const x = Math.max(0, t.x + item.box.x * t.k)
      return below + tipRoom > height ? { id, x, y: t.y + item.box.y * t.k, above: true } : { id, x, y: below }
    },
    [nav, height],
  )

  // The events of every card and line come to the frame.
  const latest = useRef({ nav, byId, model, props, reveal, tipAt, tabStop })
  useLayoutEffect(() => {
    latest.current = { nav, byId, model, props, reveal, tipAt, tabStop }
  })
  useEffect(() => {
    if (!frame) return
    const itemOf = (target: EventTarget | null) => (target instanceof Element ? target.closest<HTMLElement>('[data-item]') : null)
    const edgeOf = (target: EventTarget | null) => (target instanceof Element ? target.closest<SVGGElement>('.fm-pick[data-edge]') : null)
    const inTip = (target: EventTarget | null) => target instanceof Element && target.closest('.fm-tip') !== null
    const element = (id: string) => [...frame.querySelectorAll<HTMLElement>('[data-item]')].find((el) => el.dataset.item === id)
    const keyedNow = () => {
      const el = document.activeElement
      return el instanceof HTMLElement && frame.contains(el) ? el.dataset.item : undefined
    }
    let stepping = false
    // The tooltip stays a moment after the pointer leaves what it tells of,
    // so that the pointer can go into it.
    let leaving: ReturnType<typeof setTimeout> | undefined
    const stay = () => {
      clearTimeout(leaving)
      leaving = undefined
    }
    const activate = (id: string) => {
      const { nav, props } = latest.current
      const item = nav.byId.get(id)
      if (item?.row?.kind === 'more' || (item?.node.kind === 'group' && item.node.band === 'targets')) props.onExpand(id)
      else props.onSelect(id)
    }
    const over = (e: PointerEvent) => {
      if (inTip(e.target)) return stay()
      const item = itemOf(e.target)
      const edge = item ? null : edgeOf(e.target)
      const id = item?.dataset.item ?? edge?.dataset.edge
      if (!id) {
        if (leaving !== undefined) return
        leaving = setTimeout(() => {
          leaving = undefined
          setPointer(undefined)
          const k = keyedNow()
          setTip(k ? latest.current.tipAt(k) : undefined)
        }, 150)
        return
      }
      stay()
      setPointer(id)
      const box = frame.getBoundingClientRect()
      const x = e.clientX - box.left + 4
      const y = e.clientY - box.top
      const above = y + 4 + tipRoom > box.height
      setTip((was) => (was?.id === id ? was : item ? latest.current.tipAt(id) : { id, x, y: above ? y - 4 : y + 4, above }))
    }
    const leave = () => {
      stay()
      setPointer(undefined)
      const k = keyedNow()
      setTip(k ? latest.current.tipAt(k) : undefined)
    }
    const click = (e: MouseEvent) => {
      if (inTip(e.target)) return
      const item = itemOf(e.target)
      if (item?.dataset.item) return activate(item.dataset.item)
      // The trunk and the lines to a target have a history to show.
      const id = edgeOf(e.target)?.dataset.edge
      const { model, byId, props } = latest.current
      const edge = id === undefined ? undefined : model.edges.find((x) => x.id === id)
      if (edge && (edge.style === 'trunk' || endsAtTarget(edge, byId))) props.onSelect(edge.id)
    }
    const focusIn = (e: FocusEvent) => {
      const id = itemOf(e.target)?.dataset.item
      if (!id) return
      if (!stepping) {
        const item = latest.current.nav.byId.get(id)
        chain.current = item?.row && item.routes.length === 1 ? item.routes[0] : undefined
      }
      setFocused({ id, chain: chain.current })
      setTip(latest.current.tipAt(id))
      if (id !== latest.current.tabStop) latest.current.props.onFocus(id)
    }
    const focusOut = (e: FocusEvent) => {
      if (e.relatedTarget instanceof Node && frame.contains(e.relatedTarget)) return
      setFocused(undefined)
      setTip(undefined)
    }
    const key = (e: KeyboardEvent) => {
      const id = itemOf(e.target)?.dataset.item
      if (!id || e.altKey || e.metaKey || e.ctrlKey) return
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault()
        return activate(id)
      }
      if (e.key === 'Escape') {
        setTip(undefined)
        return
      }
      const moved = step(latest.current.nav, id, e.key, chain.current)
      if (e.key.startsWith('Arrow') || e.key === 'Home' || e.key === 'End') e.preventDefault()
      if (!moved) return
      chain.current = moved.chain
      const item = latest.current.nav.byId.get(moved.id)
      if (item) latest.current.reveal(item.box)
      stepping = true
      element(moved.id)?.focus({ preventScroll: true })
      stepping = false
    }
    frame.addEventListener('pointerover', over)
    frame.addEventListener('pointerleave', leave)
    frame.addEventListener('click', click)
    frame.addEventListener('focusin', focusIn)
    frame.addEventListener('focusout', focusOut)
    frame.addEventListener('keydown', key)
    return () => {
      stay()
      frame.removeEventListener('pointerover', over)
      frame.removeEventListener('pointerleave', leave)
      frame.removeEventListener('click', click)
      frame.removeEventListener('focusin', focusIn)
      frame.removeEventListener('focusout', focusOut)
      frame.removeEventListener('keydown', key)
    }
  }, [frame])

  const tipContent = tip ? tipOf(tip.id, model, byId, nav, extras, carried) : undefined

  // The item with the focus is described by its tooltip, when it has one.
  const described = keyed !== undefined && tip?.id === keyed && tipContent !== undefined
  useEffect(() => {
    if (!frame || !keyed || !described) return
    const el = [...frame.querySelectorAll<HTMLElement>('[data-item]')].find((x) => x.dataset.item === keyed)
    el?.setAttribute('aria-describedby', tipId)
    return () => el?.removeAttribute('aria-describedby')
  }, [frame, keyed, described, tipId])

  return (
    <div
      ref={setFrame}
      className={['fm-frame', stale && 'fm-stale', lit && 'fm-lighting'].filter(Boolean).join(' ')}
      role="group"
      aria-label={mapWords.map}
      aria-describedby={instructions}
    >
      <p id={instructions} className="sr-only">
        {mapWords.instructions}
      </p>
      <div ref={view} className="fm-view" style={{ width: layout.width, height: layout.height }}>
        <svg className="fm-lines" width={layout.width} height={layout.height} aria-hidden="true">
          {model.edges.map((edge) => {
            const ends = layout.edges.get(edge.id)
            if (!ends) return null
            const dim = lit ? !lit.edges.has(edge.id) : undefined
            if (edge.style === 'trunk') {
              return <Trunk key={edge.id} edge={edge} from={ends.from} to={ends.to} dim={dim} still={reducedMotion} since={extras?.since?.get(edge.id)} />
            }
            if (endsAtTarget(edge, byId)) return <TargetEdge key={edge.id} edge={edge} from={ends.from} to={ends.to} dim={dim} still={reducedMotion} />
            return <StateEdge key={edge.id} edge={edge} from={ends.from} to={ends.to} dim={dim} still={reducedMotion} />
          })}
        </svg>
        <div className="fm-cards">
          {model.nodes.map((node) => {
            const box = layout.nodes.get(node.id)
            if (!box) return null
            const Card = cardOf(node)
            const mine = tabStop !== undefined && nav.byId.get(tabStop)?.node.id === node.id ? tabStop : undefined
            return (
              <Card
                key={node.id}
                node={node}
                box={box}
                tab={mine}
                lit={lit ? (lit.cards.get(node.id) ?? '') : undefined}
                ring={rings.get(node.id)}
                selected={selected === node.id}
                ports={portsOf.get(node.id)}
                refused={node.kind === 'connector' && extras?.commands?.has(node.id)}
                still={node.kind === 'target' ? reducedMotion : undefined}
                rate={carried.get(node.id)?.rate}
                rateStale={carried.get(node.id)?.stale}
              />
            )
          })}
        </div>
        <svg className="fm-dots" width={layout.width} height={layout.height} aria-hidden="true">
          <g ref={layer} />
        </svg>
      </div>
      {mini && <MiniMap model={model} layout={layout} view={miniView} onPick={pick} />}
      {tip && tipContent && (
        <div
          id={tipId}
          role="tooltip"
          tabIndex={-1}
          className="fm-tip"
          style={{ left: Math.min(tip.x, Math.max(0, width - 320)), top: tip.y, transform: tip.above ? 'translateY(-100%)' : undefined }}
        >
          {tipContent}
        </div>
      )}
    </div>
  )
}

// tipOf is what the tooltip of a card, a line of a card or a line between
// them says beyond the name; nothing when there is nothing more.
function tipOf(id: string, model: Model, byId: ReadonlyMap<string, FlowNode>, nav: Nav, extras: MapExtras | undefined, carried: ReadonlyMap<string, PathFigure>): ReactNode {
  const item = nav.byId.get(id)
  if (item?.row) {
    const r = item.row
    if (r.holder) {
      return (
        <>
          Held by <Untrusted text={r.holder} />
          {r.reason && (
            <>
              : <Untrusted text={r.reason} />
            </>
          )}
        </>
      )
    }
    return r.reason ? <Untrusted text={r.reason} /> : undefined
  }
  if (item) {
    const figure = carried.get(item.node.id)
    if (figure) return `${pathFigure(figure)}: ${portNote}`
    const cmd = extras?.commands?.get(item.node.id)
    if (!cmd) return undefined
    return (
      <>
        <span>{item.node.kind === 'rogue' ? rotateWords.rogue : rotateWords.refused}</span>
        <Command cmd={cmd} />
      </>
    )
  }
  const edge = model.edges.find((e) => e.id === id)
  if (!edge) return undefined
  if (edge.style === 'trunk') {
    const figure = trunkFigure(edge)
    const concurrent = extras?.concurrent?.get(edge.id)
    const since = extras?.since?.get(edge.id)
    return (
      <>
        {edgeTip(edge, 1)}
        {figure && `: ${figure}`}
        {concurrent !== undefined && `, ${concurrent} in flight`}
        {edge.stale && since && `, ${noDataSince} ${since}`}
      </>
    )
  }
  const port = edge.port ? byId.get(edge.to)?.ports?.find((p) => p.id === edge.port) : undefined
  return edgeTip(edge, port?.routes?.length ?? 1)
}

// The view of the Overview: the map of the model the page shaped, kept
// steady from one notice to the next, with its legend.

export interface FlowPanelProps {
  model: Model // after collapse()
  selected?: string
  onSelect(id: string | undefined): void
  onExpand(id: string): void
  paused: boolean
  zoom?: ZoomAsk
}

const reducedQuery = '(prefers-reduced-motion: reduce)'

function useReducedMotion(): boolean {
  const [query] = useState(() => (typeof window.matchMedia === 'function' ? window.matchMedia(reducedQuery) : undefined))
  const [reduced, setReduced] = useState(query?.matches ?? false)
  useEffect(() => {
    if (!query) return
    const changed = () => setReduced(query.matches)
    query.addEventListener('change', changed)
    return () => query.removeEventListener('change', changed)
  }, [query])
  return reduced
}

// useMotion is the map's one animation loop, which runs while the map is
// shown and ends when it goes.
function useMotion(): Motion {
  const [motion] = useState(() => restartable(() => createMotion({ cap: dotCap })))
  useEffect(() => {
    motion.start()
    return () => motion.stop()
  }, [motion])
  return motion
}

interface Held {
  model: Model
  layout: Layout
  changed?: { ids: ReadonlySet<string>; n: number }
}

const ignore = () => undefined

export function FlowPanel({ model, selected, onSelect, onExpand, paused, zoom: ask }: FlowPanelProps): JSX.Element {
  const st = useApp((s) => s.state)
  const traffic = useApp((s) => s.traffic)
  const conn = useApp((s) => s.conn)
  const reduced = useReducedMotion()
  const stale = conn !== 'live'
  const motion = useMotion()
  useEffect(() => {
    if (paused || stale) motion.pause()
    else motion.resume()
  }, [motion, paused, stale])

  // The layout of each view starts from the one before, so that nothing
  // moves that did not change, and what changed state rings once.
  const [held, setHeld] = useState<Held & { from: Model }>(() => {
    const kept = keepSame(undefined, model)
    return { from: model, model: kept, layout: layOut(kept) }
  })
  let now = held
  if (held.from !== model) {
    const kept = keepSame(held.model, model)
    const ids = changesOf(held.model, kept)
    now = { from: model, model: kept, layout: layOut(kept, held.layout), changed: ids.size > 0 ? { ids, n: (held.changed?.n ?? 0) + 1 } : held.changed }
    setHeld(now)
  }

  const [focus, setFocus] = useState<string>()
  const extras = useMemo(() => (st ? extrasOf(st, traffic, now.model) : undefined), [st, traffic, now.model])

  // The map is as tall as its fitted width makes it, up to most of the
  // window; past that it scrolls.
  const ratio = now.layout.height / now.layout.width
  return (
    <div className="fm-panel">
      <div className="fm-box" style={{ height: `min(calc(max(100cqw, ${Math.round(now.layout.width * minFit)}px) * ${ratio.toFixed(4)}), max(360px, 75vh))` }}>
        <FlowMap
          model={now.model}
          layout={now.layout}
          motion={motion}
          selected={selected}
          focus={focus}
          onSelect={onSelect}
          onFocus={setFocus}
          onExpand={onExpand}
          onViewport={ignore}
          reducedMotion={reduced || paused}
          extras={extras}
          stale={stale}
          changed={now.changed}
          zoom={ask}
        />
      </div>
      <Legend why={traffic?.routesWhy} paused={paused} reduced={reduced} stale={stale} />
    </div>
  )
}
