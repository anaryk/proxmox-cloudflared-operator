import type { JSX } from 'react'

import type { CommandWords } from '../text/words'

// What every renderer of the flow map is given and what it gives back. The
// map of the Overview (src/flow/FlowMap.tsx, its default export) and the
// renderers of the performance harness in e2e/perf are all a FlowMap. The
// model, its layout and the animation loop are built without a renderer.

export type Band = 'hostnames' | 'edge' | 'connector' | 'path' | 'targets'

// A route is named by its key, routeKey of text/routes.ts. Nodes, rows,
// access points and edges list the routes whose chain passes through them:
// hovering one highlights those chains, and a focus keeps them.

// FlowRow is a line of a zone card: a route, a hostname of a guest that
// waits for approval, the routes a folded card leaves out ("+ 186 active")
// or a group of routes that are not active.
export interface FlowRow {
  id?: string // 'route:<key>', or the key of the folded or grouped row
  kind?: 'route' | 'unapproved' | 'more' | 'group' // a route when not set
  hostname: string
  owner: string
  guest?: string // the guest's name
  state: string
  reason?: string
  holder?: string // a route in conflict: the owner holding its hostname
  tags: string[] // '503', 'DNS', 'waits for approval'
  label?: string // a folded or grouped row: '+ 186 active', '214 unreachable: no answer on port 8080'
  routes?: string[] // a folded or grouped row: the routes it stands for
}

// FlowPort is an access point of a target card: one published port.
export interface FlowPort {
  id: string
  label: string // ':8080 http'
  state: string
  target?: string // '10.0.0.11:8080', what the egress filter counts
  level?: string
  rate?: number // connections opened per second, one figure however many routes share the port
  stale?: boolean
  routes?: string[]
}

export interface FlowNode {
  id: string
  band: Band
  kind: 'zone' | 'edge' | 'connector' | 'rogue' | 'path' | 'target' | 'group'
  label: string
  state?: string
  rows?: FlowRow[]
  ports?: FlowPort[]
  ref?: string // what the node stands for: a zone name, an account id, a guest, an address, a connector id
  lines?: string[] // the card's further lines, in order
  tags?: string[] // 'unchecked'
  counts?: Record<string, number> // a collapsed zone card: its routes by state
  routes?: string[]
}

export interface FlowEdge {
  id: string
  from: string // node ids
  to: string
  style: 'hairline' | 'trunk' | 'plain' | 'unreachable' | 'withdrawn' | 'rogue'
  lanes?: number
  rate?: number // requests per second on the trunk, connections opened per second on a port edge
  label?: string
  errors?: number // proxy errors per second, on the trunk
  port?: string // the access point of the target card the edge ends at
  muted?: boolean // greyed: what it carries is not known now (a frozen account, a tunnel not checked)
  stale?: boolean // its figure is not current: it keeps no dots
  routes?: string[]
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

// What the map shows beside the model, read from the state and the traffic.
export interface MapExtras {
  // The command that gives a tunnel a new secret, by the node of a
  // connector pco does not run or of one whose token Cloudflare refuses.
  commands?: ReadonlyMap<string, CommandWords>
  // By the id of a trunk: the requests in flight, and when its last sample
  // was read, as a time of day.
  concurrent?: ReadonlyMap<string, number>
  since?: ReadonlyMap<string, string>
}

// A change of the view the toolbar asks for; a new n asks again.
export interface ZoomAsk {
  to: 'fit' | 'in' | 'out'
  n: number
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
  reducedMotion: boolean // the dots stand still: chevrons and the figures show them
  extras?: MapExtras
  stale?: boolean // the page has no new data: the figures are greyed
  changed?: { ids: ReadonlySet<string>; n: number } // what changed state with the last notice rings once
  zoom?: ZoomAsk
}

export type FlowMap = (props: FlowMapProps) => JSX.Element
