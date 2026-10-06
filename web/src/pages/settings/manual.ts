// Reading a settings file and checking it as the daemon would, so that an
// import writes nothing until every part of it is known to be accepted: the
// settings, and each manual route by the rules of engine.manualRoute.

import type { ManualRouteView, ManualTarget, RouteOptions, Settings } from '../../api/types.gen'
import { type FieldIssue, type Limits, containsAddr, normalizeHostname, parseIPv4, parsePrefix, settingFields, validateSettings } from './validate'

export interface ImportIssue {
  where: string
  message: string
}

export interface ImportFile {
  rev?: number
  settings: Settings
  // Absent when the file has no manual routes at all, which is not the same
  // as a file with an empty list: a replace would delete every route.
  routes?: ManualRouteView[]
}

type Obj = Record<string, unknown>

const isObject = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v)

// The keys of the file: what this page writes, and what pco settings show
// --json prints besides the settings, which is not read.
const fileKeys = ['rev', 'settings', 'manualRoutes', 'readAtStart', 'limits', 'notes', 'restartNeeded']

const kinds: Record<keyof Settings, 'text' | 'flag' | 'count' | 'list' | 'pins'> = {
  gateTag: 'text',
  allowHosts: 'list',
  denyHosts: 'list',
  pollInterval: 'text',
  grace: 'text',
  trustStatic: 'flag',
  trustedCIDRs: 'list',
  manualCIDRs: 'list',
  admission: 'text',
  zonePins: 'pins',
  observeOnly: 'flag',
  identityMinimum: 'text',
  maxHostnamesPerGuest: 'count',
  reverifyInterval: 'text',
  cloudflareBudget: 'count',
}

const wants = {
  text: 'want text',
  flag: 'want true or false',
  count: 'want a whole number',
  list: 'want a list of text',
  pins: 'want an object of text',
}

const isKind = {
  text: (v: unknown) => typeof v === 'string',
  flag: (v: unknown) => typeof v === 'boolean',
  count: (v: unknown) => typeof v === 'number' && Number.isInteger(v),
  list: (v: unknown) => Array.isArray(v) && v.every((x) => typeof x === 'string'),
  pins: (v: unknown) => isObject(v) && Object.values(v).every((x) => typeof x === 'string'),
}

function readSettings(raw: unknown, issues: ImportIssue[]): Settings | undefined {
  if (raw === undefined) {
    issues.push({ where: 'settings', message: 'missing' })
    return undefined
  }
  if (!isObject(raw)) {
    issues.push({ where: 'settings', message: 'want an object' })
    return undefined
  }
  const before = issues.length
  const known = new Set<string>(settingFields.map((f) => f.name))
  for (const key of Object.keys(raw)) {
    if (!known.has(key)) issues.push({ where: `settings.${key}`, message: 'unknown setting' })
  }
  const out: Obj = {}
  for (const { name, optional } of settingFields) {
    const v = raw[name]
    if (!Object.hasOwn(raw, name)) {
      if (!optional) issues.push({ where: `settings.${name}`, message: 'missing' })
    } else if (optional && v === null && (kinds[name] === 'list' || kinds[name] === 'pins')) {
      continue
    } else if (isKind[kinds[name]](v)) {
      out[name] = v
    } else {
      issues.push({ where: `settings.${name}`, message: wants[kinds[name]] })
    }
  }
  return issues.length === before ? (out as unknown as Settings) : undefined
}

type Types = Record<string, keyof typeof isKind>

// readFields reads the keys of an object of the file, naming one it has no
// place for and one that is of another type; a key in required that is not
// there is missing. The keys in own are read by the caller.
function readFields(raw: Obj, where: string, types: Types, required: string[], issues: ImportIssue[], own: string[] = []): Obj {
  const out: Obj = {}
  for (const key of Object.keys(raw)) {
    if (!Object.hasOwn(types, key) && !own.includes(key)) issues.push({ where: `${where}.${key}`, message: 'unknown key' })
  }
  for (const [key, kind] of Object.entries(types)) {
    if (!Object.hasOwn(raw, key)) {
      if (required.includes(key)) issues.push({ where: `${where}.${key}`, message: 'missing' })
    } else if (isKind[kind](raw[key])) {
      out[key] = raw[key]
    } else {
      issues.push({ where: `${where}.${key}`, message: wants[kind] })
    }
  }
  return out
}

const targetTypes: Types = { kind: 'text', guest: 'text', scheme: 'text', addr: 'text', port: 'count' }
const optionTypes: Types = { noTLSVerify: 'flag', hostHeader: 'text', sni: 'text', via: 'text', allowNode: 'flag' }

function readRoute(raw: unknown, at: number, issues: ImportIssue[]): ManualRouteView | undefined {
  const where = `manualRoutes[${at}]`
  if (!isObject(raw)) {
    issues.push({ where, message: 'want an object' })
    return undefined
  }
  const before = issues.length
  const top = readFields(raw, where, { id: 'text', rev: 'count', hostname: 'text' }, ['id', 'hostname'], issues, ['target', 'options'])
  const part = (name: 'target' | 'options', types: Types, required: string[]): Obj => {
    const v = raw[name]
    if (v === undefined) {
      if (name === 'target') issues.push({ where: `${where}.target`, message: 'missing' })
      return {}
    }
    if (!isObject(v)) {
      issues.push({ where: `${where}.${name}`, message: 'want an object' })
      return {}
    }
    return readFields(v, `${where}.${name}`, types, required, issues)
  }
  const target = part('target', targetTypes, ['kind', 'scheme', 'port'])
  const options = part('options', optionTypes, [])
  if (issues.length > before) return undefined
  return { id: top.id as string, rev: (top.rev as number | undefined) ?? 0, hostname: top.hostname as string, target: target as unknown as ManualTarget, options: options as RouteOptions }
}

// parseImportFile reads the text of a file as far as its shape goes: JSON,
// the keys and the types the daemon decodes. What the values may be is
// checkImport's.
export function parseImportFile(text: string): { file?: ImportFile; issues: ImportIssue[] } {
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch {
    return { issues: [{ where: 'file', message: 'this is not JSON' }] }
  }
  if (!isObject(raw)) return { issues: [{ where: 'file', message: 'the file must be a JSON object' }] }
  const issues: ImportIssue[] = []
  for (const key of Object.keys(raw)) {
    if (!fileKeys.includes(key)) issues.push({ where: key, message: 'unknown key' })
  }
  const settings = readSettings(raw.settings, issues)
  let routes: ManualRouteView[] | undefined
  if (raw.manualRoutes !== undefined) {
    if (!Array.isArray(raw.manualRoutes)) {
      issues.push({ where: 'manualRoutes', message: 'want a list' })
    } else {
      routes = []
      for (const [at, r] of (raw.manualRoutes as unknown[]).entries()) {
        const route = readRoute(r, at, issues)
        if (route) routes.push(route)
      }
    }
  }
  if (raw.rev !== undefined && !isKind.count(raw.rev)) issues.push({ where: 'rev', message: wants.count })
  if (issues.length > 0 || !settings) return { issues }
  const file: ImportFile = { settings }
  if (typeof raw.rev === 'number') file.rev = raw.rev
  if (routes) file.routes = routes
  return { file, issues }
}

const idPattern = /^[a-z0-9-]{1,32}$/
const hostHeaderPattern = /^[A-Za-z0-9._:-]{1,253}$/

const ref = /^(qemu|lxc)\/([1-9][0-9]*)$/

function guestError(guest: string): string | undefined {
  const m = ref.exec(guest)
  if (m && Number(m[2]) <= 2 ** 31 - 1) return undefined
  return `guest ref ${JSON.stringify(guest)}: want qemu/<vmid> or lxc/<vmid>`
}

// routable is annotation's: an address a guest can have, which is none of
// this host, link-local, multicast or reserved.
function routable(addr: string): boolean {
  const [first = 0, second = 0] = addr.split('.').map(Number)
  return first !== 0 && first < 240 && first !== 127 && !(first === 169 && second === 254) && !(first >= 224 && first <= 239)
}

// via is a NIC from net0 to net31, or an address a guest can have, in normal
// form; undefined for anything else.
function normalizeVia(v: string): string | undefined {
  if (v.length > 3 && v.slice(0, 3).toLowerCase() === 'net') {
    const n = /^(0|[1-9][0-9]*)$/.exec(v.slice(3))
    return n && Number(n[1]) <= 31 ? `net${v.slice(3)}` : undefined
  }
  const addr = parseIPv4(v)
  return addr !== undefined && routable(addr) ? addr : undefined
}

const prefixesText = (p: readonly string[]) => (p.length > 0 ? p.join(', ') : 'none')

// validateRoute lists what engine.manualRoute and the check of the address
// refuse in r. manualCIDRs are those the route is to be checked against, or
// undefined when they are not known to be valid.
export function validateRoute(r: ManualRouteView, manualCIDRs: readonly string[] | undefined): FieldIssue[] {
  const out: FieldIssue[] = []
  const add = (field: string, message: string) => out.push({ field, message })
  const t = r.target
  if (!idPattern.test(r.id)) add('id', `id ${JSON.stringify(r.id)}: want 1 to 32 of a-z, 0-9 and -`)
  const host = normalizeHostname(r.hostname)
  if (host.error !== undefined) add('hostname', host.error)
  const https = t.scheme === 'https'
  const before = out.length
  if (t.scheme !== 'http' && !https) add('target.scheme', `target.scheme ${JSON.stringify(t.scheme)}: want http or https`)
  else if (!Number.isInteger(t.port) || t.port < 1 || t.port > 65535) add('target.port', 'target.port: want a port from 1 to 65535')
  else if (t.kind === 'guest') {
    const bad = guestError(t.guest ?? '')
    if (bad !== undefined) add('target.guest', `target.guest: ${bad}`)
    else if (t.addr) add('target.addr', 'target.addr: a route to a guest goes to the address proven for it, and takes none')
  } else if (t.kind === 'address') {
    if (t.guest) add('target.guest', 'target.guest: a route to an address names no guest')
    else if (parseIPv4(t.addr ?? '') === undefined) add('target.addr', t.addr ? `target.addr ${t.addr}: want an IPv4 address` : 'target.addr: want an IPv4 address')
  } else {
    add('target.kind', `target.kind ${JSON.stringify(t.kind)}: want guest or address`)
  }
  const targetOk = out.length === before
  const o = r.options
  if (o.allowNode && t.kind === 'guest') add('options.allowNode', 'options.allowNode: only a route to an address may point at a node')
  if (o.noTLSVerify && !https) add('options.noTLSVerify', 'options.noTLSVerify: only applies to https targets')
  else if (o.hostHeader && !hostHeaderPattern.test(o.hostHeader)) {
    add('options.hostHeader', `options.hostHeader: ${JSON.stringify(o.hostHeader)} is not a host name with an optional port`)
  } else if (o.sni) {
    const sni = normalizeHostname(o.sni)
    if (!https) add('options.sni', 'options.sni: only applies to https targets')
    else if (sni.error !== undefined || sni.value.startsWith('*.')) add('options.sni', `options.sni: ${JSON.stringify(o.sni)} is not a host name`)
  } else if (o.via) {
    if (t.kind === 'address') add('options.via', 'options.via: cannot be combined with an address in the target')
    else if (normalizeVia(o.via) === undefined) {
      add('options.via', `options.via: ${JSON.stringify(o.via)} is neither a NIC from net0 to net31 nor an IPv4 address a guest can have`)
    }
  }
  if (targetOk && manualCIDRs !== undefined && t.kind === 'address' && t.addr) {
    const addr = t.addr
    const inside = manualCIDRs.some((p) => {
      const prefix = parsePrefix(p)
      return prefix !== undefined && containsAddr(prefix, addr)
    })
    if (!inside) add('target.addr', `target.addr ${addr}: not inside the manualCIDRs of the imported settings (${prefixesText(manualCIDRs)})`)
  }
  return out
}

// normalizeRoute writes a route that passed validateRoute in the normal form
// the daemon stores: the hostname and the name of the sni in lower case, via
// as the daemon writes it, no option that is off or empty.
export function normalizeRoute(r: ManualRouteView): ManualRouteView {
  const t = r.target
  const target: ManualTarget =
    t.kind === 'guest' ? { kind: 'guest', guest: t.guest, scheme: t.scheme, port: t.port } : { kind: 'address', scheme: t.scheme, addr: t.addr, port: t.port }
  const options: RouteOptions = {}
  if (r.options.noTLSVerify) options.noTLSVerify = true
  if (r.options.hostHeader) options.hostHeader = r.options.hostHeader
  if (r.options.sni) options.sni = normalizeHostname(r.options.sni).value ?? r.options.sni
  if (r.options.via) options.via = normalizeVia(r.options.via) ?? r.options.via
  if (r.options.allowNode) options.allowNode = true
  return { id: r.id, rev: r.rev, hostname: normalizeHostname(r.hostname).value ?? r.hostname, target, options }
}

// checkImport reads a file and checks all of it, as the daemon would on the
// saves that follow: the settings by the rules of the store, with what is
// stored to know that observe-only is not left, and every route by the rules
// of a manual route, its address against the manualCIDRs of the file, which
// are in force once its settings are saved. file is there only when nothing
// is wrong.
export function checkImport(text: string, ctx: { current: Settings; limits: Limits }): { file?: ImportFile; issues: ImportIssue[] } {
  const parsed = parseImportFile(text)
  const file = parsed.file
  if (!file) return parsed
  const issues: ImportIssue[] = []
  const found = validateSettings(file.settings, ctx.limits, ctx.current)
  for (const i of found) issues.push({ where: `settings.${i.field}`, message: i.message })
  const cidrs = found.some((i) => i.field.startsWith('manualCIDRs')) ? undefined : (file.settings.manualCIDRs ?? [])
  const routes: ManualRouteView[] = []
  const firstOf = new Map<string, number>()
  for (const [at, r] of (file.routes ?? []).entries()) {
    const where = `manualRoutes[${at}] (${r.id})`
    for (const i of validateRoute(r, cidrs)) issues.push({ where, message: i.message })
    const first = firstOf.get(r.id)
    if (first !== undefined) issues.push({ where, message: `id ${r.id} is the id of manualRoutes[${first}] too` })
    else firstOf.set(r.id, at)
    routes.push(normalizeRoute(r))
  }
  if (issues.length > 0) return { issues }
  const out: ImportFile = { ...file }
  if (file.routes) out.routes = routes
  return { file: out, issues }
}
