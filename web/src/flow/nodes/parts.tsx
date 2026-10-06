import type { CSSProperties, ReactNode } from 'react'

import type { Tone } from '../../components/icons'
import type { Box, FlowNode } from '../types'

// What every card of the map is given. The parts of the highlight and of
// the ring come as text, one id a line, so that a card whose parts did not
// change is not drawn again.
export interface CardProps {
  node: FlowNode
  box: Box
  // The item of this card that holds the map's tab stop.
  tab?: string
  // Nothing highlighted when not set; '*' the whole card, '' none of it,
  // else the ids of its lit lines or access points.
  lit?: string
  // The ids of its parts that changed state with the last notice, after
  // the notice's number.
  ring?: string
  selected?: boolean
  // A zone card: the port each of its lines is served on, in their order.
  ports?: string
  // A connector whose token Cloudflare refuses.
  refused?: boolean
  // The dots stand still: an access point says its figure.
  still?: boolean
  // A path: the connections the lines into it carry, and whether their
  // figures are old.
  rate?: number
  rateStale?: boolean
}

export const sameBox = (a: Box, b: Box): boolean => a.x === b.x && a.y === b.y && a.width === b.width && a.height === b.height

export function sameCard(a: CardProps, b: CardProps): boolean {
  return (
    a.node === b.node &&
    sameBox(a.box, b.box) &&
    a.tab === b.tab &&
    a.lit === b.lit &&
    a.ring === b.ring &&
    a.selected === b.selected &&
    a.ports === b.ports &&
    a.refused === b.refused &&
    a.still === b.still &&
    a.rate === b.rate &&
    a.rateStale === b.rateStale
  )
}

export const place = (b: Box): CSSProperties => ({ transform: `translate(${b.x}px, ${b.y}px)`, width: b.width, height: b.height })

const list = (s: string | undefined): ReadonlySet<string> | undefined => (s === undefined || s === '*' ? undefined : new Set(s === '' ? [] : s.split('\n')))

// Lit reads a card's highlight: whether the card is dimmed, and whether a
// part of it is.
export function litOf(lit: string | undefined): { card: boolean; part: (id: string) => boolean } {
  const parts = list(lit)
  return { card: lit === '', part: (id) => parts !== undefined && lit !== '' && !parts.has(id) }
}

// ringOf reads which parts ring, and the key that starts the ring anew.
export function ringOf(ring: string | undefined): { key: string; has: (id: string) => boolean } {
  if (!ring) return { key: '', has: () => false }
  const [key = '', ...ids] = ring.split('\n')
  const set = new Set(ids)
  return { key, has: (id) => set.has(id) }
}

// Ring is the one-shot ring of a part whose state changed; a new key draws
// it again.
export function Ring() {
  return <span className="fm-ring" aria-hidden="true" />
}

export function cardClass(kind: string, node: FlowNode, dim: boolean, selected: boolean | undefined, ...more: (string | false | undefined)[]): string {
  return ['fm-card', `fm-${kind}`, dim && 'fm-dim', selected && 'fm-selected', node.state && `fm-state-${node.state.replace(/[^a-z-]/g, '')}`, ...more].filter(Boolean).join(' ')
}

// The props of what holds the focus: one tab stop for the whole map.
export function focusable(id: string, tab: string | undefined, label: string) {
  return { role: 'button', tabIndex: tab === id ? 0 : -1, 'data-item': id, 'aria-label': label }
}

export function Mark({ tone, children }: { tone: Tone; children: ReactNode }) {
  return <span className={`fm-icon status-${tone}`}>{children}</span>
}

export function Lines({ lines }: { lines: readonly ReactNode[] }) {
  return (
    <div className="fm-body">
      {lines.map((line, at) => (
        <div key={at} className="fm-text">
          {line}
        </div>
      ))}
    </div>
  )
}

export const verifiedTone = (state: string | undefined): Tone => (state === 'yes' ? 'ok' : state === 'no' ? 'fail' : 'warn')

// The tone of a connector by connectorText's words: inactive, not ready or
// with a flag of its token or metrics port.
export function connectorTone(state: string | undefined): Tone {
  if (!state || state.startsWith('inactive')) return 'fail'
  if (state.startsWith('active, ready') && !state.includes(', Cloudflare') && !state.includes(', its metrics')) return 'ok'
  return 'warn'
}
