// What the Overview's map takes from one view of the model to the next:
// the objects that did not change, what changed state, and what it shows
// beside the model.

import type { State, TrafficView } from '../api/types.gen'
import { partsIn } from '../components/Time'
import { type CommandWords, rotateCommand } from '../text/words'
import { rowItemId } from './keys'
import type { MotionLoop } from './motion'
import type { FlowEdge, FlowNode, MapExtras, Model, Motion, MotionEdge } from './types'

// restartable is a motion that a page can start and stop again, as React
// does with the effects of a page it checks: what the map gave it is kept
// and given to each new loop.
export function restartable(make: () => MotionLoop): Motion & { start(): void; stop(): void } {
  let loop: MotionLoop | undefined
  let layer: SVGGElement | null = null
  let edges: readonly MotionEdge[] | undefined
  let visible: ReadonlySet<string> | undefined
  let paused = false
  return {
    start() {
      if (loop) return
      loop = make()
      if (paused) loop.pause()
      if (edges) loop.setEdges(edges)
      if (visible) loop.setVisible(visible)
      loop.attach(layer)
    },
    stop() {
      loop?.stop()
      loop = undefined
    },
    attach(next) {
      layer = next
      loop?.attach(next)
    },
    setEdges(next) {
      edges = next
      loop?.setEdges(next)
    },
    setVisible(ids) {
      visible = ids
      loop?.setVisible(ids)
    },
    pause() {
      paused = true
      loop?.pause()
    },
    resume() {
      paused = false
      loop?.resume()
    },
  }
}

function reuse<T extends { id: string }>(before: readonly T[] | undefined, next: readonly T[]): T[] {
  if (!before) return [...next]
  const was = new Map(before.map((x) => [x.id, x]))
  return next.map((x) => {
    const old = was.get(x.id)
    return old !== undefined && JSON.stringify(old) === JSON.stringify(x) ? old : x
  })
}

// keepSame is next with each node and edge that is as it was replaced by
// the object of before, so that the map draws again only what changed.
export function keepSame(before: Model | undefined, next: Model): Model {
  return { nodes: reuse<FlowNode>(before?.nodes, next.nodes), edges: reuse<FlowEdge>(before?.edges, next.edges) }
}

// changesOf lists what changed state from one view to the next: the lines
// of the zone cards, the access points and the cards. What is new did not
// change; it was not there.
export function changesOf(before: Model, next: Model): Set<string> {
  const was = new Map<string, string>()
  for (const n of before.nodes) {
    if (n.state !== undefined) was.set(n.id, n.state)
    for (const r of n.rows ?? []) was.set(rowItemId(r), r.state)
    for (const p of n.ports ?? []) was.set(p.id, p.state)
  }
  const out = new Set<string>()
  const check = (id: string, state: string | undefined) => {
    const old = was.get(id)
    if (old !== undefined && state !== undefined && old !== state) out.add(id)
  }
  for (const n of next.nodes) {
    check(n.id, n.state)
    for (const r of n.rows ?? []) check(rowItemId(r), r.state)
    for (const p of n.ports ?? []) check(p.id, p.state)
  }
  return out
}

function timeOf(at: string | undefined): string | undefined {
  const when = at ? new Date(at) : undefined
  return when && !Number.isNaN(when.getTime()) ? partsIn(when).time : undefined
}

// extrasOf reads from the state and the traffic what the map shows beside
// the model: the command for a tunnel whose secret should be rotated, and
// the figures of each trunk that are not on the line.
export function extrasOf(st: State, traffic: TrafficView | undefined, model: Model): MapExtras {
  const commands = new Map<string, CommandWords>()
  const concurrent = new Map<string, number>()
  const since = new Map<string, string>()
  const tunnelOf = new Map(st.tunnels.map((t) => [t.accountId, t]))
  for (const n of model.nodes) {
    if (n.kind === 'rogue') {
      const r = st.rogueConnectors.find((x) => x.id === n.ref)
      if (r) commands.set(n.id, rotateCommand(st.tunnels, r.accountId))
    } else if (n.kind === 'connector' && n.ref !== undefined) {
      const t = tunnelOf.get(n.ref)
      const c = t?.id ? st.connectors.find((x) => x.tunnelId === t.id) : undefined
      if (c?.tokenRefused) commands.set(n.id, rotateCommand(st.tunnels, n.ref))
    }
  }
  for (const e of model.edges) {
    if (e.style !== 'trunk') continue
    const t = tunnelOf.get(e.from.slice('edge:'.length))
    const tt = t?.id ? traffic?.tunnels.find((x) => x.tunnelId === t.id) : undefined
    const last = tt?.samples.at(-1)
    if (!tt || !last) continue
    concurrent.set(e.id, last.concurrent)
    const at = timeOf(last.at)
    if (tt.stale && at) since.set(e.id, at)
  }
  return { commands, concurrent, since }
}
