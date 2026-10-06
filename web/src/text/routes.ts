// How every page names and orders routes: the key of a route and the order of
// its owners, as model.CompareOwners has it, and how its path reaches the
// guest.

import type { PathView } from '../api/types.gen'

// routeKey names a route: a hostname has one route per owner. Neither a
// hostname nor an owner holds a space, and the address of the Overview
// writes the key the same way, route:<hostname> <owner>.
export const routeKey = (r: { hostname: string; owner: string }): string => `${r.hostname} ${r.owner}`

const compareText = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)

// The rank and VMID model.ParseGuestRef reads from an owner: a VMID in its
// canonical form, from 1 to 2^31 - 1. Any other form is not a guest, so
// qemu/0101 comes after the manual routes.
function ownerKey(owner: string): [number, number] {
  const m = /^(qemu|lxc)\/([0-9]+)$/.exec(owner)
  const vmid = m ? Number(m[2]) : 0
  if (m && String(vmid) === m[2] && vmid >= 1 && vmid < 2 ** 31) return [m[1] === 'qemu' ? 0 : 1, vmid]
  if (owner.startsWith('manual/')) return [2, 0]
  return [3, 0]
}

// compareOwners is model.CompareOwners: virtual machines, then containers, by
// VMID, then the manual routes, then anything else, each by name.
export function compareOwners(a: string, b: string): number {
  const [rankA, vmidA] = ownerKey(a)
  const [rankB, vmidB] = ownerKey(b)
  return rankA - rankB || vmidA - vmidB || compareText(a, b)
}

// legOf says whether a path reaches its guest through the leg of a network,
// as PathView.Via has it: "attach:vmbr2" through the leg of an attached
// network, kind attach and network vmbr2, or "managed" through the leg of the
// managed one. A path that is direct, or says nothing, goes through none.
export function legOf(path: { via?: string } | undefined): { kind: string; network?: string } | undefined {
  const via = path?.via
  if (!via || via === 'direct') return undefined
  const at = via.indexOf(':')
  return at < 0 ? { kind: via } : { kind: via.slice(0, at), network: via.slice(at + 1) }
}

// legState says how the leg a path goes through fares, in the words of the
// path step of pco diagnose: up, with the service address the connector dials
// and the address of the guest it is forwarded to, or what is wrong.
export function legState(path: Pick<PathView, 'addr' | 'service' | 'leg'>): { ok: boolean; text: string } {
  if (!path.service) return { ok: false, text: 'no service address' }
  if (path.leg !== 'up') return { ok: false, text: `the leg is not up: ${path.leg || 'the gateway has not said how it is'}` }
  return { ok: true, text: `leg up, service ${path.service}${path.addr ? ` → ${path.addr}` : ''}` }
}
