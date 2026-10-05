import type { JSX } from 'react'

// What every renderer of the flow map is given and what it gives back. The
// map of the Overview (src/flow/FlowMap.tsx, its default export) and the
// renderers of the performance harness in e2e/perf are all a FlowMap. The
// model, its layout and the animation loop are built without a renderer.

export type Band = 'hostnames' | 'edge' | 'connector' | 'path' | 'targets'

export interface FlowRow {
  hostname: string
  owner: string
  state: string
  tags: string[] // '503', 'DNS', 'waits for approval'
}

// FlowPort is an access point of a target card: one published port.
export interface FlowPort {
  id: string
  label: string // ':8080 http'
  state: string
}

export interface FlowNode {
  id: string
  band: Band
  kind: 'zone' | 'edge' | 'connector' | 'rogue' | 'path' | 'target' | 'group'
  label: string
  state?: string
  rows?: FlowRow[]
  ports?: FlowPort[]
}

export interface FlowEdge {
  id: string
  from: string // node ids
  to: string
  style: 'hairline' | 'trunk' | 'plain' | 'unreachable' | 'withdrawn' | 'rogue'
  lanes?: number
  rate?: number // requests per second on the trunk, connections opened per second on a port edge
  label?: string
}

export interface Model {
  nodes: FlowNode[]
  edges: FlowEdge[]
}

export interface Point {
  x: number
  y: number
}

export interface Box {
  x: number
  y: number
  width: number
  height: number
}

// Layout is in layout units, the bands spanning 1244 of them; the renderer
// scales them to the screen.
export interface Layout {
  width: number
  height: number
  nodes: ReadonlyMap<string, Box>
  // Where each edge leaves its card and where it ends: a card, a row of a
  // zone card or an access point of a target card.
  edges: ReadonlyMap<string, { from: Point; to: Point }>
}

// MotionEdge is an edge as the map drew it: at(t) is the point at the
// fraction t of its length, in layout units.
export interface MotionEdge {
  edge: FlowEdge
  at(t: number): Point
}

// Motion is the one animation loop of the map. It draws the dots into the
// group the map attaches, which is in layout units like the edges, and gives
// an edge dots by its rate.
export interface Motion {
  attach(layer: SVGGElement | null): void
  setEdges(edges: readonly MotionEdge[]): void
  setVisible(ids: ReadonlySet<string>): void // the edges in the viewport; the others get no dots
  pause(): void
  resume(): void
}

export interface FlowMapProps {
  model: Model // after collapse()
  layout: Layout // positions from layout()
  motion: Motion // createMotion(); the map registers its edge paths with it
  selected?: string // node id shown in the drawer
  focus?: string // node id with keyboard focus (roving tabindex)
  onSelect(id: string | undefined): void
  onFocus(id: string): void
  onExpand(id: string): void // a folded group or "+ N active" row opened
  onViewport(v: { x: number; y: number; zoom: number }): void // culling and the URL query
  reducedMotion: boolean
}

export type FlowMap = (props: FlowMapProps) => JSX.Element
