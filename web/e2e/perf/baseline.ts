// The baseline of the performance test (spec-ui 12.2): the map drawn with
// React and the DOM alone, the cards as absolutely positioned elements, the
// edges in one SVG, the dots moved by the page's loop. A renderer is measured
// against it in the same run, so what it costs beyond this is its own.

import './baseline.css'

import { createElement as h, type JSX, memo, useCallback, useEffect, useLayoutEffect, useState } from 'react'

import { FailIcon, GlobeIcon, GuestIcon, NetworkIcon, OkIcon, TunnelIcon, WithdrawnIcon } from '../../src/components/icons'
import type { Box, FlowEdge, FlowMapProps, FlowNode, Layout, MotionEdge, Point } from '../../src/flow/types'

// An edge is the mockup's curve: it leaves and enters horizontally, its
// control points halfway across.
export function edgePath(from: Point, to: Point): string {
  const mid = (from.x + to.x) / 2
  return `M${from.x} ${from.y}C${mid} ${from.y} ${mid} ${to.y} ${to.x} ${to.y}`
}

export function edgeAt(from: Point, to: Point): (t: number) => Point {
  const mid = (from.x + to.x) / 2
  return (t) => {
    const u = 1 - t
    const a = u * u * u
    const b = 3 * u * u * t
    const c = 3 * u * t * t
    const d = t * t * t
    return { x: a * from.x + (b + c) * mid + d * to.x, y: (a + b) * from.y + (c + d) * to.y }
  }
}

// movingEdges are the edges with a rate, as the motion loop takes them.
export function movingEdges(edges: readonly FlowEdge[], layout: Layout): MotionEdge[] {
  return edges.flatMap((edge) => {
    const ends = layout.edges.get(edge.id)
    return ends && edge.rate ? [{ edge, at: edgeAt(ends.from, ends.to) }] : []
  })
}

const stateIcon = (state: string) => (state === 'unreachable' ? FailIcon : state === 'withdrawn' ? WithdrawnIcon : OkIcon)
const kindIcon = { zone: GlobeIcon, edge: GlobeIcon, connector: TunnelIcon, rogue: TunnelIcon, path: NetworkIcon, target: GuestIcon, group: GlobeIcon }

// CardBody is what a card shows: a header, the rows of a zone card and the
// access points of a target card.
export const CardBody = memo(function CardBody({ node }: { node: FlowNode }): JSX.Element {
  return h(
    'div',
    { className: `card ${node.kind}` },
    h('div', { className: 'head' }, h(kindIcon[node.kind]), h('span', { className: 'label' }, node.label), h('span', { className: 'meta' }, node.state)),
    node.rows?.map((r) =>
      h(
        'div',
        { key: r.hostname, className: 'row', 'data-state': r.state },
        h(stateIcon(r.state)),
        h('span', { className: 'host' }, r.hostname),
        r.tags.map((tag) => h('span', { key: tag, className: 'tag' }, tag)),
        h('span', { className: 'owner' }, r.owner),
      ),
    ),
    node.ports?.map((p) =>
      h(
        'div',
        { key: p.id, className: 'port', 'data-state': p.state },
        h(stateIcon(p.state)),
        h('span', { className: 'label' }, p.label),
        h('span', { className: 'meta' }, p.state),
      ),
    ),
  )
})

interface PlacedProps {
  node: FlowNode
  box: Box
  tabbable: boolean
  onSelect(id: string | undefined): void
  onFocus(id: string): void
}

const Placed = memo(function Placed({ node, box, tabbable, onSelect, onFocus }: PlacedProps): JSX.Element {
  return h(
    'div',
    {
      className: 'place',
      style: { transform: `translate(${box.x}px, ${box.y}px)`, width: box.width, height: box.height },
      tabIndex: tabbable ? 0 : -1,
      'aria-label': node.label,
      onClick: () => onSelect(node.id),
      onFocus: () => onFocus(node.id),
    },
    h(CardBody, { node }),
  )
})

const Line = memo(function Line({ edge, from, to }: { edge: FlowEdge; from: Point; to: Point }): JSX.Element {
  const d = edgePath(from, to)
  if (edge.style === 'trunk') return h('g', null, h('path', { className: 'edge pipe', d }), h('path', { className: 'edge', d }))
  return h('path', { className: `edge ${edge.style}`, d })
})

export default function Baseline({ model, layout, motion, focus, onSelect, onFocus, onViewport }: FlowMapProps): JSX.Element {
  const [frame, setFrame] = useState<HTMLDivElement | null>(null)
  const [zoom, setZoom] = useState(1)

  // Fit the width, as the map does.
  useLayoutEffect(() => {
    const el = frame
    if (!el) return
    const fit = () => {
      const k = el.clientWidth / layout.width
      setZoom(k)
      onViewport({ x: 0, y: 0, zoom: k })
    }
    fit()
    const watch = new ResizeObserver(fit)
    watch.observe(el)
    return () => watch.disconnect()
  }, [frame, layout.width, onViewport])

  const layer = useCallback((g: SVGGElement | null) => motion.attach(g), [motion])
  useEffect(() => motion.setEdges(movingEdges(model.edges, layout)), [model.edges, layout, motion])

  const tabStop = focus ?? model.nodes[0]?.id
  return h(
    'div',
    { ref: setFrame, className: 'baseline' },
    h(
      'div',
      { className: 'view', style: { width: layout.width, height: layout.height, transform: `scale(${zoom})` } },
      h(
        'svg',
        { className: 'lines', width: layout.width, height: layout.height, 'aria-hidden': true },
        model.edges.map((edge) => {
          const ends = layout.edges.get(edge.id)
          return ends && h(Line, { key: edge.id, edge, from: ends.from, to: ends.to })
        }),
        h('g', { ref: layer, className: 'dots' }),
      ),
      model.nodes.map((node) => {
        const box = layout.nodes.get(node.id)
        return box && h(Placed, { key: node.id, node, box, tabbable: node.id === tabStop, onSelect, onFocus })
      }),
    ),
  )
}
