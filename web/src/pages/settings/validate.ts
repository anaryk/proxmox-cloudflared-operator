// The rules of store.Settings.normalized and internal/hostname, written again
// for the form, so that a mistake shows at its field before anything is sent.
// The daemon checks every save and its answer wins; the ranges are not
// constants of this file but the limits it reports.

import type { Limit, Settings } from '../../api/types.gen'
import { longestDuration, parseDuration } from '../../text/duration'
import { goQuote, goQuoteRune } from '../../text/quote'

export interface FieldIssue {
  // The path of the field in the JSON of the settings, as the daemon names
  // it: "pollInterval", "denyHosts[2]", zonePins["example.com"].
  field: string
  message: string
}

// The fields of store.Settings in the order it declares them, and those its
// JSON may leave out (omitempty). The generated types say the same, which a
// test checks.
export const settingFields: readonly { name: keyof Settings; optional: boolean }[] = [
  { name: 'gateTag', optional: false },
  { name: 'allowHosts', optional: true },
  { name: 'denyHosts', optional: true },
  { name: 'pollInterval', optional: false },
  { name: 'grace', optional: false },
  { name: 'trustStatic', optional: true },
  { name: 'trustedCIDRs', optional: true },
  { name: 'manualCIDRs', optional: true },
  { name: 'admission', optional: false },
  { name: 'zonePins', optional: true },
  { name: 'observeOnly', optional: false },
  { name: 'identityMinimum', optional: false },
  { name: 'maxHostnamesPerGuest', optional: false },
  { name: 'reverifyInterval', optional: false },
  { name: 'cloudflareBudget', optional: false },
]

export type Limits = Readonly<Record<string, Limit>>

export type Normalized = { value: string; error?: undefined } | { value?: undefined; error: string }

const maxNameLen = 253
const maxLabelLen = 63
const maxTagLen = 64

const label = (l: string): string | undefined => {
  if (l === '') return 'empty label'
  if (l.length > maxLabelLen) return `label ${goQuote(l)} is longer than ${maxLabelLen} characters`
  for (const c of l) {
    if (!/^[A-Za-z0-9-]$/.test(c)) return `label ${goQuote(l)} contains ${goQuoteRune(c)}`
  }
  if (l.startsWith('-') || l.endsWith('-')) return `label ${goQuote(l)} starts or ends with a hyphen`
  return undefined
}

function labelsError(labels: string[]): string | undefined {
  for (const l of labels) {
    const e = label(l)
    if (e) return e
  }
  const last = labels[labels.length - 1] ?? ''
  return /^[0-9]+$/.test(last) ? `last label ${goQuote(last)} is all digits` : undefined
}

function nameError(h: string): string | undefined {
  if (h === '') return 'empty'
  if (h.length > maxNameLen) return `longer than ${maxNameLen} characters`
  let labels = h.split('.')
  if (labels[0] === '*') {
    labels = labels.slice(1)
    if (labels.length < 2) return 'a wildcard needs at least two labels after the *'
  } else if (labels.length < 2) {
    return 'needs at least two labels'
  }
  return labelsError(labels)
}

const withoutDot = (s: string) => (s.endsWith('.') ? s.slice(0, -1) : s)

// normalizeHostname is hostname.Normalize: lower case, one trailing dot
// stripped, a leading "*." label allowed.
export function normalizeHostname(s: string): Normalized {
  const h = withoutDot(s)
  const error = nameError(h)
  return error === undefined ? { value: h.toLowerCase() } : { error: `invalid hostname "${s}": ${error}` }
}

// normalizePattern is hostname.NormalizePattern: "*", a hostname, or "*."
// and labels, so that "*.com" is a pattern though no hostname.
export function normalizePattern(s: string): Normalized {
  if (s === '*') return { value: s }
  const p = withoutDot(s)
  let error: string | undefined
  if (p === '*') error = 'a bare * takes no trailing dot'
  else if (p.startsWith('*.') && !p.slice(2).includes('.')) error = labelsError([p.slice(2)])
  else error = nameError(p)
  return error === undefined ? { value: p.toLowerCase() } : { error }
}

const octet = '(?:0|[1-9][0-9]{0,2})'
const ipv4 = new RegExp(`^${octet}(?:\\.${octet}){3}$`)

// parseIPv4 is the address when s is a dotted IPv4 address as netip reads
// one: four numbers of 0 to 255 without leading zeros.
export function parseIPv4(s: string): string | undefined {
  if (!ipv4.test(s)) return undefined
  return s.split('.').every((o) => Number(o) <= 255) ? s : undefined
}

export interface Prefix {
  addr: number
  bits: number
}

const toNumber = (a: string) => a.split('.').reduce((n, o) => n * 256 + Number(o), 0)

// parsePrefix reads "10.0.5.0/24", as netip.ParsePrefix does for IPv4. The
// bits of the address below the length are kept in the text, as the store
// keeps them, and ignored when the prefix is asked for an address.
export function parsePrefix(s: string): Prefix | undefined {
  const at = s.indexOf('/')
  if (at < 0) return undefined
  const addr = parseIPv4(s.slice(0, at))
  const bits = /^(0|[1-9][0-9]?)$/.exec(s.slice(at + 1))?.[1]
  if (addr === undefined || bits === undefined || Number(bits) > 32) return undefined
  return { addr: toNumber(addr), bits: Number(bits) }
}

export function containsAddr(p: Prefix, addr: string): boolean {
  const size = 2 ** (32 - p.bits)
  return Math.floor(p.addr / size) === Math.floor(toNumber(addr) / size)
}

const tagPattern = /^[a-z0-9_][a-z0-9_\-+.]*$/

function tagError(tag: string): string | undefined {
  if (tag.length > maxTagLen) return `longer than ${maxTagLen} characters`
  if (!tagPattern.test(tag)) return 'want lower-case letters, digits and the characters _ - + ., the first one not - + or a dot'
  return undefined
}

const durationOf = (v: unknown) => (typeof v === 'string' ? parseDuration(v) : undefined)

function checkDuration(field: 'pollInterval' | 'grace' | 'reverifyInterval', text: string, limits: Limits): string | undefined {
  const ms = parseDuration(text)
  if (ms === undefined) return `${field} ${JSON.stringify(text)}: write a duration as 10s or 1m30s, of at most ${longestDuration}`
  const { min, max } = limits[field] ?? {}
  const lowest = durationOf(min)
  if (lowest !== undefined && ms < lowest) return `${field} ${text}: at least ${String(min)}`
  const highest = durationOf(max)
  if (highest !== undefined && ms > highest) return `${field} ${text}: at most ${String(max)}`
  return undefined
}

// The largest whole number the daemon reads into a setting, a Go int.
const largestCount = '9223372036854775807'

function checkCount(field: 'maxHostnamesPerGuest' | 'cloudflareBudget', n: number, limits: Limits): string | undefined {
  if (!Number.isInteger(n)) return `${field}: want a whole number`
  if (Math.abs(n) >= 2 ** 63) return `${field}: want a whole number of at most ${largestCount}`
  const { min, max } = limits[field] ?? {}
  const low = typeof min === 'number' ? min : undefined
  const high = typeof max === 'number' ? max : undefined
  if (low !== undefined && high !== undefined) return n < low || n > high ? `${field} ${n}: from ${low} to ${high}` : undefined
  if (low !== undefined && n < low) return `${field} ${n}: at least ${low}`
  if (high !== undefined && n > high) return `${field} ${n}: at most ${high}`
  return undefined
}

function checkPatterns(field: 'allowHosts' | 'denyHosts', list: readonly string[] | undefined, out: FieldIssue[]): void {
  for (const [i, p] of (list ?? []).entries()) {
    const n = normalizePattern(p)
    if (n.error !== undefined) out.push({ field: `${field}[${i}]`, message: `${field}[${i}] ${JSON.stringify(p)}: ${n.error}` })
  }
}

function checkPrefixes(field: 'trustedCIDRs' | 'manualCIDRs', list: readonly string[] | undefined, out: FieldIssue[]): void {
  for (const [i, p] of (list ?? []).entries()) {
    if (parsePrefix(p) === undefined) out.push({ field: `${field}[${i}]`, message: `${field}[${i}] ${p}: want an IPv4 prefix` })
  }
}

function checkPins(pins: Readonly<Record<string, string>> | undefined, out: FieldIssue[]): void {
  const seen = new Set<string>()
  for (const zone of Object.keys(pins ?? {}).sort()) {
    const field = `zonePins[${JSON.stringify(zone)}]`
    const name = normalizeHostname(zone)
    if (name.error !== undefined) out.push({ field, message: `zonePins: ${name.error}` })
    else if (pins?.[zone] === '') out.push({ field, message: `${field}: the credential id is empty` })
    else if (seen.has(name.value)) out.push({ field, message: `${field}: another key names zone ${name.value} too` })
    else seen.add(name.value)
  }
}

// validateSettings lists what the daemon would refuse in s, in the order it
// checks: every field once, every entry of a list. was, the settings as
// stored, says whether leaving observe-only is asked, which is not a setting
// but an apply.
export function validateSettings(s: Settings, limits: Limits, was?: Settings): FieldIssue[] {
  const out: FieldIssue[] = []
  const add = (field: string, message: string | undefined) => {
    if (message !== undefined) out.push({ field, message })
  }
  if (was?.observeOnly && !s.observeOnly) {
    add('observeOnly', 'observeOnly: leaving observe-only mode is not a setting, so that what it changes is shown first; use apply (pco apply)')
  }
  const tag = tagError(s.gateTag)
  add('gateTag', tag === undefined ? undefined : `gateTag ${JSON.stringify(s.gateTag)}: ${tag}`)
  checkPatterns('allowHosts', s.allowHosts, out)
  checkPatterns('denyHosts', s.denyHosts, out)
  add('pollInterval', checkDuration('pollInterval', s.pollInterval, limits))
  add('grace', checkDuration('grace', s.grace, limits))
  add('maxHostnamesPerGuest', checkCount('maxHostnamesPerGuest', s.maxHostnamesPerGuest, limits))
  add('reverifyInterval', checkDuration('reverifyInterval', s.reverifyInterval, limits))
  add('cloudflareBudget', checkCount('cloudflareBudget', s.cloudflareBudget, limits))
  if (s.admission !== 'tag' && s.admission !== 'approve') {
    add('admission', `admission ${JSON.stringify(s.admission)}: want "tag" or "approve"`)
  }
  if (s.identityMinimum !== 'observed' && s.identityMinimum !== 'filtered' && s.identityMinimum !== 'port') {
    add('identityMinimum', `identityMinimum ${JSON.stringify(s.identityMinimum)}: want "observed", "filtered" or "port"`)
  }
  checkPrefixes('trustedCIDRs', s.trustedCIDRs, out)
  checkPrefixes('manualCIDRs', s.manualCIDRs, out)
  checkPins(s.zonePins, out)
  return out
}
