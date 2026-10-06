// The words of the flow map: its controls, its legend, what its lines and
// cards say beside their figures, and the name each node gives a screen
// reader. A name is an attribute, which cannot isolate text, so whatever a
// guest or Cloudflare wrote goes into it with its controls marked.

import type { FlowEdge, FlowNode, FlowPort, FlowRow } from '../flow/types'
import { marked } from './chars'

export const mapWords = {
  map: 'Flow map',
  skip: 'Skip the map',
  instructions:
    'The arrow keys move along the chains, Home and End to the first and the last column, Enter opens the details. The list view has the same content as text.',
  listView: 'List view',
  fit: 'Fit',
  zoomIn: 'Zoom in',
  zoomOut: 'Zoom out',
  pause: 'Pause motion',
  resume: 'Resume motion',
  expandAll: 'Expand all',
  foldAgain: 'Fold again',
  loading: 'Drawing the map',
  bands: 'hostname → edge → connector → path → target',
} as const

// The legend row: the dots and where their figures come from, the line
// styles, and what moves.
export const legendWords = {
  label: 'Legend',
  trunkDots: 'Requests and proxy errors, from the connector’s own counters',
  portDots: 'Connections the connector opened to a target, from the egress filter’s counters',
  served: 'Served',
  unreachable: 'Unreachable',
  withdrawn: 'Withdrawn (503, DNS kept)',
  rogue: 'Connector pco does not run',
  muted: 'Greyed: not checked, frozen or no new data',
  moves: 'Only lines with a measured figure move: dot density follows the rate, errors are red diamonds. Hostname lines never move.',
  still: 'Motion is paused: chevrons and the figure stand for the dots.',
  reduced: 'Reduced motion: chevrons and the figure stand for the dots.',
  stale: 'No new data: the figures are the last ones read, and nothing moves.',
  noCounters: 'Port lines show state only:',
} as const

// The reasons the daemon gives for having no figures per target are the
// legend's words; a failed read adds what went wrong, which the legend
// keeps for its tooltip.
const unread = 'the counters could not be read'

export function countersWhy(why: string): { short: string; detail?: string } {
  if (why.startsWith(`${unread}: `)) return { short: unread, detail: why.slice(unread.length + 2) }
  return { short: why }
}

export const figure = (n: number): string => (Number.isFinite(n) ? n.toFixed(1) : '-')

export function trunkFigure(e: Pick<FlowEdge, 'rate' | 'errors'>): string {
  if (e.rate === undefined) return ''
  const errors = e.errors ?? 0
  return errors > 0 ? `${figure(e.rate)} req/s · ${figure(errors)} errors/s` : `${figure(e.rate)} req/s`
}

export const connectionsFigure = (rate: number): string => `${figure(rate)} new connections per second`

// The figure beside the chevrons of a still line, where there is little room.
export const connectionsShort = (rate: number): string => `${figure(rate)} conn/s`

export const portNote = 'new connections from the connector, not requests'

export const sharedNote = (n: number): string => `shared by ${n} routes`

export const noDataSince = 'no data since'

// The tooltip of an edge past the connector: its figure, what it counts and
// with whom it is shared.
export function edgeTip(e: FlowEdge, shared: number): string {
  if (e.style === 'hairline') return e.label ?? ''
  if (e.style === 'rogue') return 'A connector pco does not run, on the tunnel of this edge'
  if (e.style === 'trunk') return 'Traffic between the edge and the connector'
  const parts: string[] = []
  if (e.rate !== undefined) parts.push(`${connectionsFigure(e.rate)}: ${portNote}`)
  else if (e.style === 'withdrawn') parts.push('Withdrawn: the rule answers 503 and the DNS record is kept')
  else if (e.style === 'unreachable') parts.push('Unreachable: the target does not answer')
  else parts.push('No figure for this line')
  if (shared > 1) parts.push(sharedNote(shared))
  if (e.stale) parts.push('no new samples')
  return parts.join(', ')
}

const servedStates = new Set(['active', 'unreachable', 'withdrawn', 'frozen', 'held'])

function tagWords(tags: readonly string[]): string[] {
  return tags.flatMap((t) => {
    if (t === '503') return ['answers 503']
    if (t === 'DNS') return ['a DNS record of someone else holds the name']
    if (t === 'waits for approval') return []
    return [marked(t)]
  })
}

function who(owner: string, guest?: string): string {
  return guest ? `${marked(owner)} ${marked(guest)}` : marked(owner)
}

// rowLabel names a line of a zone card: "www.example.com, active, served by
// qemu/101 web-1 on port 8080".
export function rowLabel(r: FlowRow, port?: string): string {
  if (r.kind === 'more') return `${marked(r.label ?? '')}, opens the rest of the card`
  if (r.kind === 'group') return `${marked(r.label ?? '')}, lists their routes`
  const parts = [marked(r.hostname)]
  const state = r.kind === 'unapproved' ? 'waits for approval' : r.state
  parts.push(r.reason ? `${marked(state)}: ${marked(r.reason)}` : marked(state))
  if (r.holder) parts.push(`held by ${marked(r.holder)}`)
  parts.push(...tagWords(r.tags))
  const by = r.kind !== 'unapproved' && servedStates.has(r.state) ? 'served by' : 'asked for by'
  parts.push(`${by} ${who(r.owner, r.guest)}${port ? ` on port ${port}` : ''}`)
  return parts.join(', ')
}

function countsText(counts: Record<string, number> | undefined): string {
  return Object.entries(counts ?? {})
    .map(([state, n]) => `${n} ${marked(state)}`)
    .join(', ')
}

const routeRows = (n: FlowNode) => (n.rows ?? []).filter((r) => r.kind === undefined || r.kind === 'route' || r.kind === 'unapproved')

function hostnames(n: number): string {
  return n === 1 ? '1 hostname' : `${n} hostnames`
}

function portText(p: FlowPort): string {
  const figure = p.rate === undefined ? '' : `, ${connectionsFigure(p.rate)}${p.stale ? ', no new samples' : ''}`
  return `${marked(p.label)} ${marked(p.state)}${figure}`
}

// nodeLabel names a card or the head of a zone card by what it is, its
// state and its lines.
export function nodeLabel(n: FlowNode): string {
  const lines = (n.lines ?? []).map(marked)
  switch (n.kind) {
    case 'zone': {
      const size = n.counts ? countsText(n.counts) : hostnames(routeRows(n).length)
      if (!n.ref) return `Hostnames in no zone, ${size}`
      const account = lines[0] ? `, account ${lines[0]}` : ''
      return `Zone ${marked(n.label)}${n.state ? `, ${marked(n.state)}` : ''}${account}, ${size}`
    }
    case 'edge':
      return `Edge of tunnel ${marked(n.label)}, verified: ${marked(n.state ?? 'unknown')}${lines.length > 0 ? `, ${lines.join(', ')}` : ''}`
    case 'connector': {
      const [tunnel, ...rest] = lines
      const unchecked = n.tags?.includes('unchecked') ? ', not checked in the last cycle' : ''
      return `Connector on ${marked(n.label)}${tunnel ? ` for tunnel ${tunnel}` : ''}: ${marked(n.state ?? '')}${rest.length > 0 ? `, ${rest.join(', ')}` : ''}${unchecked}`
    }
    case 'rogue':
      return `Connector ${marked(n.label)}${lines.length > 0 ? `, ${lines.join(', ')}` : ''}`
    case 'path':
      return `Path ${marked(n.label)}`
    case 'target': {
      const ports = (n.ports ?? []).map(portText)
      const what = n.id.startsWith('address:') ? `Address ${marked(n.label)} of ${lines[0] ?? ''}` : `Guest ${marked(n.label)} ${lines[0] ?? ''}`
      return `${what.trim()}${ports.length > 0 ? `, ports ${ports.join('; ')}` : ''}`
    }
    case 'group':
      if (n.band === 'hostnames') return `${marked(n.label)}: ${countsText(n.counts)}, lists their routes`
      return `${marked(n.label)}, ${marked(n.state ?? '')}, opens them`
  }
  return marked(n.label)
}
