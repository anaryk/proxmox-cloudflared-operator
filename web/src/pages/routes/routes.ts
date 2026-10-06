// What the routes pages work out from the state by themselves: the order of
// the CLI, the filters, the holder of a hostname, the path and the links.

import type { PathView, RouteView } from '../../api/types.gen'
import { compareRouteStates } from '../../text/words'

export const manualPrefix = 'manual/'

export const isManual = (owner: string) => owner.startsWith(manualPrefix)

const compareText = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)

// The rank and VMID model.ParseGuestRef reads from an owner: a VMID in its
// canonical form, from 1 to 2^31 - 1.
function ownerKey(owner: string): [number, number] {
  const m = /^(qemu|lxc)\/([0-9]+)$/.exec(owner)
  const vmid = m ? Number(m[2]) : 0
  if (m && String(vmid) === m[2] && vmid >= 1 && vmid < 2 ** 31) return [m[1] === 'qemu' ? 0 : 1, vmid]
  if (isManual(owner)) return [2, 0]
  return [3, 0]
}

// compareOwners is model.CompareOwners: virtual machines, then containers, by
// VMID, then the manual routes, then anything else, each by name.
export function compareOwners(a: string, b: string): number {
  const [rankA, vmidA] = ownerKey(a)
  const [rankB, vmidB] = ownerKey(b)
  return rankA - rankB || vmidA - vmidB || compareText(a, b)
}

// compareRoutes is the order of pco routes: hostname, then owner.
export function compareRoutes(a: RouteView, b: RouteView): number {
  return compareText(a.hostname, b.hostname) || compareOwners(a.owner, b.owner)
}

export const routeKey = (r: { hostname: string; owner: string }) => `${r.hostname}\u0000${r.owner}`

export interface RouteFilter {
  states: readonly string[] // none: every state
  zone: string // '': every zone
  text: string
}

export const noFilter: RouteFilter = { states: [], zone: '', text: '' }

// matchesRoute says whether a route passes the filter. The text is looked for
// in the hostname, the owner, the guest's name and the service.
export function matchesRoute(r: RouteView, f: RouteFilter): boolean {
  if (f.states.length > 0 && !f.states.includes(r.state)) return false
  if (f.zone && (r.zone ?? '') !== f.zone) return false
  const text = f.text.trim().toLowerCase()
  if (!text) return true
  return [r.hostname, r.owner, r.guest?.name, r.service].some((v) => v?.toLowerCase().includes(text))
}

// stateCounts counts the routes in each state: those of present.RouteStateOrder
// in its order, with a zero where there is none, then any other state.
export function stateCounts(routes: readonly RouteView[], order: readonly string[]): [string, number][] {
  const counts = new Map<string, number>(order.map((s) => [s, 0]))
  for (const r of routes) counts.set(r.state, (counts.get(r.state) ?? 0) + 1)
  return [...counts.entries()].sort(([a], [b]) => compareRouteStates(a, b))
}

// holderOf is doctor.HolderOf: the route of host that did not lose it to
// another owner, or failing that any route of host. It is the route the
// daemon diagnoses.
export function holderOf(routes: readonly RouteView[], host: string): RouteView | undefined {
  const same = routes.filter((r) => r.hostname.toLowerCase() === host.toLowerCase())
  return same.find((r) => r.state !== 'conflict') ?? same[0]
}

export const noBridge = 'no bridge proven'

// pathText is the path a route was proven on: the node, the bridge, the VLAN
// as configured on the guest's NIC and the port. A route without a binding,
// as a manual route, or one whose proof placed the MAC on no bridge has no
// bridge proven.
export function pathText(path: PathView | undefined): string {
  if (!path) return noBridge
  if (!path.bridge) return path.node ? `${path.node} · ${noBridge}` : noBridge
  return [path.node, path.bridge, path.vlan ? `VLAN ${path.vlan}` : '', path.port ?? ''].filter(Boolean).join(' · ')
}

// The names a link may open: exact names, never a wildcard nor anything
// that could be read as more than a hostname.
const exactName = /^([a-z0-9-]{1,63}\.)+[a-z0-9-]{2,63}$/

export function openableURL(hostname: string): string | undefined {
  return exactName.test(hostname) ? `https://${hostname}/` : undefined
}

export const isWildcard = (hostname: string) => hostname.startsWith('*.')

export function routeLink(hostname: string, owner?: string): string {
  return `/routes/${encodeURIComponent(hostname)}${owner ? `?owner=${encodeURIComponent(owner)}` : ''}`
}

// allowHostLink opens the settings with the hostname of a rejected route
// added to allowHosts, nothing saved. The pattern is the route's own
// hostname, never a word of its reason. A manual route is never rejected.
export function allowHostLink(r: RouteView): string | undefined {
  if (r.state !== 'rejected' || isManual(r.owner)) return undefined
  return `/settings?addAllowHost=${encodeURIComponent(r.hostname)}&owner=${encodeURIComponent(r.owner)}`
}

// ownerName is engine.OwnerName: the owner, with the guest's name after it.
export function ownerName(owner: string, guest?: { name?: string }): string {
  return guest?.name ? `${owner} (${guest.name})` : owner
}

// Go writes a time it never set as the zero time, or leaves it out.
export const unset = (at?: string) => !at || at.startsWith('0001-01-01T00:00:00')
