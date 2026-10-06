// The form of a manual route, checked as the daemon checks it (the store's
// and the planner's rules, in their words), so that most mistakes are said
// at the field before anything is sent. The daemon's answer still wins.

import type { ManualRouteView } from '../../api/types.gen'
import { goQuote, goQuoteRune } from '../../text/quote'

export interface ManualValues {
  id: string
  hostname: string
  kind: 'guest' | 'address'
  guest: string
  addr: string
  port: string
  scheme: 'http' | 'https'
  noTLSVerify: boolean
  hostHeader: string
  sni: string
  via: string
  allowNode: boolean
}

export type ManualField = keyof ManualValues
export type ManualErrors = Partial<Record<ManualField, string>>

export function valuesOf(r?: ManualRouteView): ManualValues {
  const t = r?.target
  const o = r?.options
  return {
    id: r?.id ?? '',
    hostname: r?.hostname ?? '',
    kind: t?.kind === 'guest' ? 'guest' : 'address',
    guest: t?.guest ?? '',
    addr: t?.addr ?? '',
    port: t ? String(t.port) : '',
    scheme: t?.scheme === 'https' ? 'https' : 'http',
    noTLSVerify: o?.noTLSVerify ?? false,
    hostHeader: o?.hostHeader ?? '',
    sni: o?.sni ?? '',
    via: o?.via ?? '',
    allowNode: o?.allowNode ?? false,
  }
}

const maxName = 253
const maxLabel = 63

// hostnameError is why hostname.Normalize refuses a name, in its words, or
// undefined for a name it takes.
export function hostnameError(raw: string, wildcard = true): string | undefined {
  const h = raw.replace(/\.$/, '')
  if (h === '') return 'empty'
  if (h.length > maxName) return `longer than ${maxName} characters`
  let labels = h.split('.')
  if (labels[0] === '*') {
    if (!wildcard) return 'not a host name'
    labels = labels.slice(1)
    if (labels.length < 2) return 'a wildcard needs at least two labels after the *'
  } else if (labels.length < 2) {
    return 'needs at least two labels'
  }
  for (const l of labels) {
    if (l === '') return 'empty label'
    if (l.length > maxLabel) return `label ${goQuote(l)} is longer than ${maxLabel} characters`
    const bad = [...l].find((c) => !/^[a-zA-Z0-9-]$/.test(c))
    if (bad !== undefined) return `label ${goQuote(l)} contains ${goQuoteRune(bad)}`
    if (l.startsWith('-') || l.endsWith('-')) return `label ${goQuote(l)} starts or ends with a hyphen`
  }
  const last = labels[labels.length - 1] ?? ''
  if (/^[0-9]+$/.test(last)) return `last label ${goQuote(last)} is all digits`
  return undefined
}

export const normalHostname = (raw: string) => raw.trim().replace(/\.$/, '').toLowerCase()

// parseIPv4 reads an address as netip.ParseAddr does: four decimal numbers
// up to 255 without leading zeros. It answers the address as a number.
export function parseIPv4(s: string): number | undefined {
  const parts = s.split('.')
  if (parts.length !== 4) return undefined
  let n = 0
  for (const p of parts) {
    if (!/^(0|[1-9][0-9]{0,2})$/.test(p) || Number(p) > 255) return undefined
    n = n * 256 + Number(p)
  }
  return n
}

// inPrefix says whether an IPv4 address is inside a prefix such as
// 10.0.5.0/24; a prefix of IPv6 holds no IPv4 address.
export function inPrefix(addr: number, prefix: string): boolean {
  const [net, bits] = prefix.split('/')
  const base = parseIPv4(net ?? '')
  const size = Number(bits)
  if (base === undefined || !/^[0-9]{1,2}$/.test(bits ?? '') || size > 32) return false
  const block = 2 ** (32 - size)
  return Math.floor(addr / block) === Math.floor(base / block)
}

const viaNIC = /^net(0|[1-9][0-9]?)$/i
const maxNIC = 31

// A via that names an address takes one a guest can have.
function routable(addr: number): boolean {
  const first = Math.floor(addr / 2 ** 24)
  const second = Math.floor(addr / 2 ** 16) % 256
  return first !== 0 && first < 224 && first !== 127 && !(first === 169 && second === 254)
}

export function viaError(via: string): string | undefined {
  const nic = viaNIC.exec(via)
  if (nic && Number(nic[1]) <= maxNIC) return undefined
  const addr = parseIPv4(via)
  if (addr !== undefined && routable(addr)) return undefined
  return `"${via}" is neither a NIC from net0 to net${maxNIC} nor an IPv4 address a guest can have`
}

export interface ValidateOptions {
  isNew: boolean
  // The manualCIDRs of the settings; undefined while they are not known,
  // and the daemon checks the address alone.
  manualCIDRs?: readonly string[]
}

export function validate(v: ManualValues, o: ValidateOptions): ManualErrors {
  const e: ManualErrors = {}
  if (o.isNew && v.id !== '' && !/^[a-z0-9-]{1,32}$/.test(v.id)) e.id = 'want 1 to 32 of a-z, 0-9 and -, or nothing for an id of its own'
  const host = hostnameError(v.hostname.trim())
  if (host) e.hostname = host
  if (!/^[0-9]{1,5}$/.test(v.port.trim()) || Number(v.port) < 1 || Number(v.port) > 65535) e.port = 'want a port from 1 to 65535'
  if (v.kind === 'guest') {
    if (!/^(qemu|lxc)\/[1-9][0-9]*$/.test(v.guest)) e.guest = 'choose a guest'
    if (v.via.trim()) {
      const via = viaError(v.via.trim())
      if (via) e.via = via
    }
  } else {
    const addr = parseIPv4(v.addr.trim())
    if (addr === undefined) {
      e.addr = 'want an IPv4 address'
    } else if (o.manualCIDRs !== undefined && !o.manualCIDRs.some((p) => inPrefix(addr, p))) {
      e.addr = `not inside the manualCIDRs of the settings (${o.manualCIDRs.length > 0 ? o.manualCIDRs.join(', ') : 'none'})`
    }
  }
  if (v.hostHeader.trim() && !/^[A-Za-z0-9._:-]{1,253}$/.test(v.hostHeader.trim())) {
    e.hostHeader = `"${v.hostHeader.trim()}" is not a host name with an optional port`
  }
  if (v.scheme === 'https' && v.sni.trim() && hostnameError(v.sni.trim(), false)) e.sni = `"${v.sni.trim()}" is not a host name`
  return e
}

// bodyOf is what is sent: only the options that apply to the target, and
// the id of a new route only when one was given.
export function bodyOf(v: ManualValues, isNew: boolean, rev?: number): Partial<ManualRouteView> {
  const https = v.scheme === 'https'
  const options: ManualRouteView['options'] = {}
  if (https && v.noTLSVerify) options.noTLSVerify = true
  if (v.hostHeader.trim()) options.hostHeader = v.hostHeader.trim()
  if (https && v.sni.trim()) options.sni = v.sni.trim()
  if (v.kind === 'guest' && v.via.trim()) options.via = v.via.trim()
  if (v.kind === 'address' && v.allowNode) options.allowNode = true
  const port = Number(v.port.trim())
  return {
    ...(isNew && v.id ? { id: v.id } : {}),
    ...(rev ? { rev } : {}),
    hostname: normalHostname(v.hostname),
    target: v.kind === 'guest' ? { kind: 'guest', scheme: v.scheme, guest: v.guest, port } : { kind: 'address', scheme: v.scheme, addr: v.addr.trim(), port },
    options,
  }
}

// The fields of the daemon's answer, by the JSON path it names.
export const fieldOfPath: Readonly<Record<string, ManualField>> = {
  id: 'id',
  hostname: 'hostname',
  'target.kind': 'kind',
  'target.guest': 'guest',
  'target.addr': 'addr',
  'target.port': 'port',
  'target.scheme': 'scheme',
  'options.noTLSVerify': 'noTLSVerify',
  'options.hostHeader': 'hostHeader',
  'options.sni': 'sni',
  'options.via': 'via',
  'options.allowNode': 'allowNode',
}

// needsNodeQuestion says whether saving publishes a service of a node that
// was not published so before: allowNode set on a route that did not have
// it for this address, port and scheme.
export function needsNodeQuestion(v: ManualValues, before?: ManualRouteView): boolean {
  if (v.kind !== 'address' || !v.allowNode) return false
  const t = before?.target
  return !(before?.options.allowNode && t?.kind === 'address' && t.addr === v.addr.trim() && t.port === Number(v.port) && t.scheme === v.scheme)
}

export const serviceOf = (v: ManualValues) => `${v.scheme}://${v.addr.trim()}:${v.port.trim()}`
