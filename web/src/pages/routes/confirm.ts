// The dialog that confirms what waits, as a machine of states. The daemon
// names what a confirmation accepts with the offer of one state and refuses
// any other: the dialog sends the offer of the list on screen, and never one
// the admin has not read.

import type { ApplyResult, Waiting } from '../../api/types.gen'

// A line of what waits: the sentence of an entry, or one of its items.
export interface Line {
  kind: string
  subject: string
  text: string
}

export type ConfirmState =
  | { name: 'reviewing'; offer: string; items: readonly Waiting[]; error?: string }
  | { name: 'typing'; offer: string; items: readonly Waiting[]; typed: string; error?: string }
  | { name: 'sending'; offer: string; items: readonly Waiting[]; typed: string }
  | { name: 'changed'; oldOffer: string; newOffer: string; added: readonly Line[]; removed: readonly Line[]; items: readonly Waiting[] }
  | { name: 'refused'; message: string; items: readonly Waiting[] }
  | { name: 'done'; accepted: readonly Waiting[]; leftObserveOnly: boolean }

export type ConfirmEvent =
  // The stream brought a state: what waits in it, and its offer.
  | { type: 'state'; offer: string; waiting: readonly Waiting[] }
  | { type: 'type'; text: string }
  | { type: 'send' }
  | { type: 'answer'; result: ApplyResult }
  // A refusal of the daemon, which says what changed; another error leaves
  // the list on screen, with what went wrong.
  | { type: 'refused'; message: string }
  | { type: 'failed'; message: string }
  // "Review the new list": the list and the offer of the state now.
  | { type: 'review'; offer: string; waiting: readonly Waiting[] }

// The kinds that remove something for good: the admin types the word.
const removing = ['dns-removals', 'vanished-guests']

export const confirmWord = 'confirm'

export function needsWord(items: readonly Waiting[]): boolean {
  return items.some((w) => removing.includes(w.kind))
}

export function start(offer: string | undefined, waiting: readonly Waiting[]): ConfirmState {
  return { name: 'reviewing', offer: offer ?? '', items: waiting }
}

// canSend says whether the confirm button sends: there is a list and an
// offer, and the word is typed where one is asked for.
export function canSend(s: ConfirmState): boolean {
  if (s.name !== 'reviewing' && s.name !== 'typing') return false
  if (s.items.length === 0 || s.offer === '') return false
  if (!needsWord(s.items)) return true
  return s.name === 'typing' && s.typed.trim() === confirmWord
}

// request is the body of the confirmation: the offer of the reviewed list.
export function request(s: Extract<ConfirmState, { name: 'sending' }>): { confirmDeletes: true; offer: string } {
  return { confirmDeletes: true, offer: s.offer }
}

export function linesOf(waiting: readonly Waiting[]): Line[] {
  return waiting.flatMap((w) => [
    { kind: w.kind, subject: w.subject, text: w.detail },
    ...(w.items ?? []).map((text) => ({ kind: w.kind, subject: w.subject, text })),
  ])
}

const lineKey = (l: Line) => `${l.kind}\u0000${l.subject}\u0000${l.text}`

// difference is what the new list adds to the one reviewed, and what it no
// longer has.
export function difference(before: readonly Waiting[], after: readonly Waiting[]): { added: Line[]; removed: Line[] } {
  const was = linesOf(before)
  const now = linesOf(after)
  const wasKeys = new Set(was.map(lineKey))
  const nowKeys = new Set(now.map(lineKey))
  return { added: now.filter((l) => !wasKeys.has(lineKey(l))), removed: was.filter((l) => !nowKeys.has(lineKey(l))) }
}

export function reduce(s: ConfirmState, e: ConfirmEvent): ConfirmState {
  switch (e.type) {
    case 'state':
      if (s.name === 'reviewing' || s.name === 'typing') {
        if (e.offer === s.offer) return s
        return { name: 'changed', oldOffer: s.offer, newOffer: e.offer, ...difference(s.items, e.waiting), items: s.items }
      }
      // Once changed it stays so until the new list is reviewed, even
      // should the offer come back: the old one is never sent.
      if (s.name === 'changed') return { ...s, newOffer: e.offer, ...difference(s.items, e.waiting) }
      // What is sent is in the daemon's hands; its answer says.
      return s
    case 'type':
      if (s.name !== 'reviewing' && s.name !== 'typing') return s
      return { name: 'typing', offer: s.offer, items: s.items, typed: e.text }
    case 'send':
      if (!canSend(s) || (s.name !== 'reviewing' && s.name !== 'typing')) return s
      return { name: 'sending', offer: s.offer, items: s.items, typed: s.name === 'typing' ? s.typed : '' }
    case 'answer':
      if (s.name !== 'sending') return s
      return { name: 'done', accepted: e.result.accepted ?? [], leftObserveOnly: e.result.leftObserveOnly }
    case 'refused':
      if (s.name !== 'sending') return s
      return { name: 'refused', message: e.message, items: s.items }
    case 'failed':
      if (s.name !== 'sending') return s
      return s.typed ? { name: 'typing', offer: s.offer, items: s.items, typed: s.typed, error: e.message } : { name: 'reviewing', offer: s.offer, items: s.items, error: e.message }
    case 'review':
      if (s.name !== 'changed' && s.name !== 'refused') return s
      return start(e.offer, e.waiting)
  }
  return s
}

const counted = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`

function listed(parts: readonly string[]): string {
  if (parts.length < 2) return parts.join('')
  return `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`
}

// effect names what the confirmation does, for its button: "Accept 6
// removals and 2 vanished guests at the next run".
export function effect(items: readonly Waiting[]): string {
  const of = (kind: string) => items.filter((w) => w.kind === kind)
  const itemsOf = (kind: string) => of(kind).reduce((n, w) => n + Math.max((w.items ?? []).length, 1), 0)
  const parts: string[] = []
  const removals = itemsOf('dns-removals')
  if (removals) parts.push(counted(removals, 'removal', 'removals'))
  const vanished = itemsOf('vanished-guests')
  if (vanished) parts.push(counted(vanished, 'vanished guest', 'vanished guests'))
  const zones = of('stale-zone').length
  if (zones) parts.push(counted(zones, 'zone that is gone', 'zones that are gone'))
  const tunnels = of('unseen-tunnel').length
  if (tunnels) parts.push(counted(tunnels, 'tunnel that is gone', 'tunnels that are gone'))
  const known = ['dns-removals', 'vanished-guests', 'stale-zone', 'unseen-tunnel']
  const other = items.filter((w) => !known.includes(w.kind)).length
  if (other) parts.push(counted(other, 'other entry', 'other entries'))
  return parts.length > 0 ? `Accept ${listed(parts)} at the next run` : 'Nothing to accept'
}
