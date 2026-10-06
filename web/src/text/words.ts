// The words of internal/present that take logic, written again for the
// page. The lookups come from words.gen.ts; both are checked against
// internal/present/testdata/words.json, so the page and the command line say
// the same.

import {
  argForms,
  egressWords,
  rateLimitWait,
  routeStateOrder,
  verifiedWords,
  waitingReads,
  writerWords,
} from '../gen/words.gen'

// The fields of the daemon's answers that the words read, named after their
// Go types.
export interface StateView {
  at?: string
  mode?: string
  complete?: boolean
  problems?: readonly string[] | null
  credentials?: readonly unknown[] | null
  waiting?: readonly Waiting[] | null
  actions?: readonly Action[] | null
}
export interface Waiting {
  kind: string
  items?: readonly string[] | null
}
export interface Action {
  kind: string
  target: string
  destructive?: boolean
  applied?: boolean
}
export interface EgressView {
  state?: string
}
export interface TunnelView {
  accountId: string
  credentialId?: string
  name?: string
  id?: string
  exists?: boolean
  verified?: boolean
  unknown?: boolean
  held?: string
  leftAsIs?: boolean
  unchecked?: boolean
}
export interface ConnectorStatus {
  active?: boolean
  ready?: boolean
  connections?: number
  tokenRefused?: boolean
  metricsPortHeld?: boolean
}
export interface CredentialView {
  checked?: boolean
  report?: {
    usable?: boolean
    checks?: readonly { ok?: boolean; unanswered?: boolean }[] | null
  }
}
export interface ApprovalView {
  matches?: boolean
  current?: string
}
export interface RouteView {
  reason?: string
  warnings?: readonly string[] | null
}

// Go writes a time it never set as the zero time, or leaves it out.
const unset = (at?: string) => !at || at.startsWith('0001-01-01T00:00:00')

export function modeText(st: StateView): string {
  if (unset(st.at)) return 'unknown'
  if (st.mode === 'observe') return 'observe-only'
  return st.mode ?? ''
}

export function inventoryText(st: StateView): string {
  if (unset(st.at)) return 'unknown'
  return st.complete ? 'complete' : 'incomplete'
}

export function egressText(v: EgressView): string {
  const state = v.state ?? ''
  return egressWords.get(state) ?? state
}

export function writerText(verdict: string): string {
  return writerWords.get(verdict) ?? verdict
}

export function verifiedText(t: TunnelView): string {
  if (t.unchecked) return verifiedWords.unchecked
  if (t.held) return verifiedWords.held
  if (t.unknown) return verifiedWords.unknown
  return t.verified ? verifiedWords.verified : verifiedWords.unverified
}

export function connectorText(s: ConnectorStatus): string {
  const n = s.connections ?? 0
  let text = !s.active ? 'inactive' : !s.ready ? 'active, not ready' : n === 1 ? 'active, ready, 1 connection' : `active, ready, ${n} connections`
  if (s.tokenRefused) text += ', Cloudflare refuses its token'
  if (s.metricsPortHeld) text += ', its metrics port is held by another process'
  return text
}

// A report that cannot say what the token can do: not usable, and every
// check that failed got no answer.
function unanswered(report: CredentialView['report']): boolean {
  if (!report || report.usable) return false
  let failed = false
  for (const check of report.checks ?? []) {
    if (check.ok) continue
    if (!check.unanswered) return false
    failed = true
  }
  return failed
}

export function credentialState(v: CredentialView): 'usable' | 'problem' | 'unknown' {
  if (!v.checked || unanswered(v.report)) return 'unknown'
  return v.report?.usable ? 'usable' : 'problem'
}

export function identityNow(v: ApprovalView): string {
  if (v.matches) return 'the same'
  if (!v.current) return 'not in the last listing'
  return `changed to ${v.current}: approve it again to publish it`
}

export function routeNote(r: RouteView): string {
  return r.reason || (r.warnings?.[0] ?? '')
}

export function nextStep(st: StateView): string {
  if (unset(st.at) || (st.problems ?? []).some((p) => p.includes('pco setup'))) return ''
  if ((st.credentials ?? []).length === 0) return 'Add a Cloudflare token with pco credential add.'
  if (st.mode === 'observe') return 'Run pco apply to start publishing.'
  return ''
}

// unaffected lists the destructive actions of the last cycle that were not
// carried out and that a confirmation does not let through.
export function unaffected<A extends Action>(st: { waiting?: readonly Waiting[] | null; actions?: readonly A[] | null }): A[] {
  const offered = new Set<string>()
  for (const w of st.waiting ?? []) {
    if (w.kind === 'dns-removals') {
      for (const name of w.items ?? []) offered.add(name)
    }
  }
  return (st.actions ?? []).filter((a) => !a.applied && a.destructive && !(a.kind === 'delete-record' && offered.has(a.target)))
}

// compareRouteStates orders states as present.RouteStateOrder, and the
// states it does not list after them, by name.
export function compareRouteStates(a: string, b: string): number {
  const rank = (s: string) => {
    const at = routeStateOrder.indexOf(s)
    return at < 0 ? routeStateOrder.length : at
  }
  return rank(a) - rank(b) || (a < b ? -1 : a > b ? 1 : 0)
}

const making = Symbol('command')

// Command is a command for a root shell that this module made: a constant,
// or words with values that passed commandArg. The class is not exported and
// its constructor takes a key only this module holds, so a page cannot hand
// CopyCommand a string it put together itself: a string does not type-check
// as one, and an object cast to one fails isCommand, which CopyCommand asks.
class Command {
  readonly #text: string

  constructor(key: symbol, text: string) {
    if (key !== making) throw new TypeError('a command is made in words.ts only')
    this.#text = text
  }

  static is(value: unknown): value is Command {
    return typeof value === 'object' && value !== null && #text in value
  }

  get text(): string {
    return this.#text
  }
}

export type { Command }

export function isCommand(value: unknown): value is Command {
  return Command.is(value)
}

const command = (text: string) => new Command(making, text)

// CommandWords is a command, or why there is none.
export type CommandWords = { command: Command; refused?: undefined } | { command?: undefined; refused: string }

export const egressOnCommand: CommandWords = { command: command('pco egress on') }
export const egressLoadCommand: CommandWords = { command: command('pco egress load') }
export const setupCommand: CommandWords = { command: command('pco setup') }

// commandArg is present.CommandArg: value when it has the form of its kind,
// else why not. A value that begins with a dash is refused whatever its
// kind, as a command would take it for an option.
export function commandArg(kind: string, value: string): { value: string; refused: string } {
  if (value.startsWith('-')) return { value: '', refused: `the ${kind} begins with a dash, which a command would take for an option` }
  const form = argForms.get(kind)
  if (form !== undefined && new RegExp(form).test(value)) return { value, refused: '' }
  return { value: '', refused: `the ${kind} has an unexpected form` }
}

// The list of present's errors: sorted, each once, the last after "and".
function andList(items: readonly string[]): string {
  const sorted = [...new Set(items)].sort()
  return sorted.length < 2 ? sorted.join('') : `${sorted.slice(0, -1).join(', ')} and ${sorted[sorted.length - 1]}`
}

// rotateCommand is present.RotateCommand: the command that rotates the secret
// of the tunnel in account, or the daemon's reason to refuse it.
export function rotateCommand(tunnels: readonly TunnelView[], account: string): CommandWords {
  const arg = commandArg('account id', account)
  if (arg.refused) return { refused: arg.refused }
  const found = tunnels.filter((t) => t.exists && t.id && t.credentialId && !t.unknown && t.accountId === arg.value)
  const [first] = found
  if (!first) {
    return { refused: `not found: no tunnel of this install is known in account ${arg.value}; pco status lists the tunnels` }
  }
  if (found.length > 1) {
    const accounts = andList(found.map((t) => t.accountId))
    return { refused: `invalid request: the install has tunnels in accounts ${accounts}; name one with --account` }
  }
  if (first.leftAsIs) {
    return {
      refused: `refused: tunnel ${first.name ?? ''} in account ${first.accountId} is left as it is: ${first.held ?? ''}; nothing was changed`,
    }
  }
  return { command: command(`pco tunnel rotate --account ${arg.value}`) }
}

// The line reconcile.Waiting writes for what waits for Cloudflare's rate limit.
function waitingLine(changes: number, reads: readonly string[]): string {
  const parts = [...reads]
  if (changes === 1) parts.push('1 change')
  else if (changes > 1) parts.push(`${changes} changes`)
  const verb = parts.length === 1 && changes <= 1 ? 'waits' : 'wait'
  if (parts.length === 0) return ''
  const last = parts.pop() ?? ''
  return `${parts.length > 0 ? `${parts.join(', ')} and ` : ''}${last} ${verb} ${rateLimitWait}`
}

function changesOf(part: string): number | undefined {
  if (part === '1 change') return 1
  const count = part.endsWith(' changes') ? part.slice(0, -' changes'.length) : undefined
  return count !== undefined && /^[+-]?[0-9]+$/.test(count) ? Number(count) : undefined
}

// budgetWait is present.BudgetWait: whether a problem line is the one that
// says what waits for Cloudflare's rate limit, and how many changes wait. The
// line is read back in full, so a message that only ends in the same words is
// not taken for it.
export function budgetWait(line: string): { changes: number; matched: boolean } {
  const no = { changes: 0, matched: false }
  const ending = [` waits ${rateLimitWait}`, ` wait ${rateLimitWait}`].find((e) => line.endsWith(e))
  if (ending === undefined) return no
  let parts = line.slice(0, -ending.length).split(', ')
  const last = parts[parts.length - 1] ?? ''
  const and = last.indexOf(' and ')
  if (and >= 0) parts = [...parts.slice(0, -1), last.slice(0, and), last.slice(and + ' and '.length)]
  const changes = changesOf(parts[parts.length - 1] ?? '')
  if (changes !== undefined) parts = parts.slice(0, -1)
  if (!parts.every((p) => waitingReads.some((read) => p.startsWith(read)))) return no
  if (waitingLine(changes ?? 0, parts) !== line) return no
  return { changes: changes ?? 0, matched: true }
}
