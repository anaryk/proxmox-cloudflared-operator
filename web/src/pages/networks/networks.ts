import type { RouteView } from '../../api/types.gen'

// A network as the routes were proven on it: a bridge and the VLAN tag of the
// guest's card, which is what the guest is configured with, not something the
// proof saw. The one without a bridge holds the routes that were proven but
// not on a bridge.
export interface Network {
  key: string
  bridge?: string
  vlan?: number
  routes: RouteView[]
}

export interface Networks {
  networks: Network[]
  // routes none of whose addresses was proven: they lost their hostname, were
  // refused or are held
  unproven: number
}

// A route has a proof when a level says so, or it is a manual route, which
// the admin who wrote it vouches for.
const proven = (r: RouteView) => Boolean(r.level) || r.owner.startsWith('manual/') || r.path !== undefined

// networksOf places each route on the bridge and VLAN its path names: one
// network each, by bridge and then VLAN with the untagged first, and last the
// network without a bridge for the routes that have none, manual routes among
// them.
export function networksOf(routes: readonly RouteView[]): Networks {
  const by = new Map<string, Network>()
  let unproven = 0
  for (const r of routes) {
    const bridge = r.path?.bridge
    if (!bridge && !proven(r)) {
      unproven++
      continue
    }
    const vlan = bridge ? r.path?.vlan : undefined
    const key = bridge ? `net:${bridge}:${vlan ?? ''}` : 'net:none'
    let n = by.get(key)
    if (!n) {
      n = { key, bridge, vlan, routes: [] }
      by.set(key, n)
    }
    n.routes.push(r)
  }
  const networks = [...by.values()].sort((a, b) => {
    if (!a.bridge || !b.bridge) return Number(!a.bridge) - Number(!b.bridge)
    return a.bridge < b.bridge ? -1 : a.bridge > b.bridge ? 1 : (a.vlan ?? -1) - (b.vlan ?? -1)
  })
  return { networks, unproven }
}

// The levels in the order of what they prove; one that is not listed follows
// by name.
const levelOrder = ['port', 'filtered', 'observed', 'manual']

// levelsOf counts the routes of a network by their level, the strongest first.
export function levelsOf(routes: readonly RouteView[]): [string, number][] {
  const counts = new Map<string, number>()
  for (const r of routes) {
    const level = r.level || 'none'
    counts.set(level, (counts.get(level) ?? 0) + 1)
  }
  const rank = (level: string) => {
    const at = levelOrder.indexOf(level)
    return at < 0 ? levelOrder.length : at
  }
  return [...counts].sort(([a], [b]) => rank(a) - rank(b) || (a < b ? -1 : a > b ? 1 : 0))
}

export interface GuestRef {
  ref: string
  name?: string
}

// guestsOf are the guests of the routes of a network, each once, by owner.
export function guestsOf(routes: readonly RouteView[]): GuestRef[] {
  const by = new Map<string, GuestRef>()
  for (const r of routes) {
    if (r.guest) by.set(`${r.guest.kind}/${r.guest.vmid}`, { ref: `${r.guest.kind}/${r.guest.vmid}`, name: r.guest.name })
  }
  return [...by.values()].sort((a, b) => (a.ref < b.ref ? -1 : a.ref > b.ref ? 1 : 0))
}
