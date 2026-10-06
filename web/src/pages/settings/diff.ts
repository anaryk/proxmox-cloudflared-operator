// What a save or an import changes, in words: settings against the settings
// stored, manual routes against the manual routes stored.

import type { ManualRouteView, ManualTarget, RouteView, Settings } from '../../api/types.gen'
import { normalizePattern, settingFields } from './validate'

export interface Change {
  field: string
  before: string
  after: string
  // For a list, a set of entries or the pins of zones: what came and went.
  added?: string[]
  removed?: string[]
}

interface Shown {
  text: string
  // The entries, for a field that is a list or a map.
  items?: string[]
}

const none = 'none'

const pinned = (pins: Readonly<Record<string, string>> | undefined) =>
  Object.entries(pins ?? {})
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([zone, id]) => `${zone} → ${id}`)

const listed = (items: string[]): Shown => ({ text: items.length > 0 ? items.join(', ') : none, items })

function shown(s: Settings, name: keyof Settings): Shown {
  switch (name) {
    case 'allowHosts':
    case 'denyHosts':
    case 'trustedCIDRs':
    case 'manualCIDRs':
      return listed([...(s[name] ?? [])])
    case 'zonePins':
      return listed(pinned(s.zonePins))
    case 'trustStatic':
    case 'observeOnly':
      return { text: String(s[name] === true) }
  }
  return { text: String(s[name]) }
}

// diffSettings lists the fields in which after differs from before, in the
// order of the settings. The order of the entries of a list is no change.
export function diffSettings(before: Settings, after: Settings): Change[] {
  const out: Change[] = []
  for (const { name } of settingFields) {
    const a = shown(before, name)
    const b = shown(after, name)
    if (a.items && b.items) {
      const removed = a.items.filter((x) => !b.items?.includes(x))
      const added = b.items.filter((x) => !a.items?.includes(x))
      if (removed.length > 0 || added.length > 0) out.push({ field: name, before: a.text, after: b.text, added, removed })
    } else if (a.text !== b.text) {
      out.push({ field: name, before: a.text, after: b.text })
    }
  }
  return out
}

// targetText writes where a manual route goes: http://10.0.5.20:9000, or
// https://qemu/101:8443 for a guest.
export function targetText(t: ManualTarget): string {
  return `${t.scheme}://${t.kind === 'guest' ? (t.guest ?? '') : (t.addr ?? '')}:${t.port}`
}

export const routeText = (r: ManualRouteView) => `${r.hostname} → ${targetText(r.target)}`

const optionNames = ['noTLSVerify', 'hostHeader', 'sni', 'via', 'allowNode'] as const

const optionText = (r: ManualRouteView, name: (typeof optionNames)[number]) => {
  const v = r.options[name]
  if (name === 'noTLSVerify' || name === 'allowNode') return String(v === true)
  return v ? String(v) : none
}

// routeChanges names what differs between two versions of a route.
export function routeChanges(before: ManualRouteView, after: ManualRouteView): string[] {
  const out: string[] = []
  if (before.hostname !== after.hostname) out.push(`hostname ${before.hostname} → ${after.hostname}`)
  const [a, b] = [targetText(before.target), targetText(after.target)]
  if (a !== b) out.push(`target ${a} → ${b}`)
  for (const name of optionNames) {
    const [x, y] = [optionText(before, name), optionText(after, name)]
    if (x !== y) out.push(`${name} ${x} → ${y}`)
  }
  return out
}

export interface RoutePlan {
  add: ManualRouteView[]
  update: { before: ManualRouteView; after: ManualRouteView; changes: string[] }[]
  same: ManualRouteView[]
  // Only for a replace: the routes the file does not have.
  remove: ManualRouteView[]
}

// planRoutes sets the routes of a file against those stored, by id. A merge
// adds and updates; a replace deletes the others as well. The revision of a
// route in the file is the revision of another install, and no difference.
export function planRoutes(current: readonly ManualRouteView[], incoming: readonly ManualRouteView[], mode: 'merge' | 'replace'): RoutePlan {
  const byId = new Map(current.map((r) => [r.id, r]))
  const plan: RoutePlan = { add: [], update: [], same: [], remove: [] }
  for (const after of incoming) {
    const before = byId.get(after.id)
    if (!before) {
      plan.add.push(after)
      continue
    }
    const changes = routeChanges(before, after)
    if (changes.length === 0) plan.same.push(before)
    else plan.update.push({ before, after, changes })
  }
  if (mode === 'replace') {
    const kept = new Set(incoming.map((r) => r.id))
    plan.remove = current.filter((r) => !kept.has(r.id))
  }
  return plan
}

// The states of a route whose hostname is published now: it has a rule in a
// tunnel, and its record.
const published = new Set(['active', 'unreachable', 'withdrawn', 'held', 'frozen'])

// matchesPattern is hostname.MatchPattern: "*" matches every name, "*.x" the
// names below x and itself, any other pattern the name it is.
export function matchesPattern(pattern: string, host: string): boolean {
  if (pattern === host || pattern === '*') return true
  if (!pattern.startsWith('*.')) return false
  const below = pattern.slice(1)
  return host.length > below.length && host.endsWith(below)
}

export type PublishedRoute = Pick<RouteView, 'hostname' | 'owner' | 'state'>

// firstAllowed says what a save does that gives allowHosts its first entry.
// Until then every hostname a guest names may be published but the apex of a
// zone and a wildcard; from then on only those a pattern matches. It is the
// routes of guests published now that no pattern of after matches, which
// stop, or undefined when the save gives the list no first entry. Manual
// routes are root's own, and the list has no say in them.
export function firstAllowed(before: Settings, after: Settings, routes: readonly PublishedRoute[]): PublishedRoute[] | undefined {
  if ((before.allowHosts ?? []).length > 0 || (after.allowHosts ?? []).length === 0) return undefined
  const patterns = (after.allowHosts ?? []).flatMap((p) => normalizePattern(p).value ?? [])
  return routes.filter((r) => !r.owner.startsWith('manual/') && published.has(r.state) && !patterns.some((p) => matchesPattern(p, r.hostname)))
}

// optInWords says what a pattern in allowHosts lets through, for the
// hostname of a route that waits for it (docs/operations.md).
export function optInWords(pattern: string, owner: string): string {
  const who = `Whoever may edit the Notes of ${owner}, or of any other tagged guest, may then publish ${pattern}`
  if (pattern.startsWith('*.')) {
    return `${who}: the wildcard that answers every name below ${pattern.slice(2)} that has no record of its own, with a valid certificate.`
  }
  return `${who}, the apex of its zone, with a valid certificate.`
}
