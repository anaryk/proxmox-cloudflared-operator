import { memo } from 'react'

import { Badge } from '../../components/Badge'
import { GuestIcon, RouteIcon } from '../../components/icons'
import { routeLook } from '../../components/StateBadge'
import { Untrusted } from '../../components/Untrusted'
import { connectionsShort, nodeLabel } from '../../text/flow'
import type { FlowPort } from '../types'
import { cardClass, type CardProps, focusable, litOf, Mark, place, Ring, ringOf, sameCard } from './parts'

function Port({ port, dim, ring, still }: { port: FlowPort; dim: boolean; ring?: string; still?: boolean }) {
  const look = routeLook(port.state)
  const shared = port.routes && port.routes.length > 1 ? `${port.routes.length}×` : undefined
  const figure = still && !port.stale && (port.rate ?? 0) > 0 ? connectionsShort(port.rate ?? 0) : undefined
  return (
    <div className={dim ? 'fm-port fm-dim' : 'fm-port'} data-state={port.state} data-port={port.id}>
      <Mark tone={look.tone}>
        <look.Icon />
      </Mark>
      <span className="fm-port-label">
        <Untrusted text={port.label} />
      </span>
      {shared && <span className="fm-shared">{shared}</span>}
      {figure && <span className="fm-rate">{figure}</span>}
      {port.state === 'withdrawn' ? (
        <Badge tone="warn">503</Badge>
      ) : (
        port.level && (
          <Badge outline>
            <Untrusted text={port.level} />
          </Badge>
        )
      )}
      {ring && <Ring key={ring} />}
    </div>
  )
}

// TargetCard is where routes end: a guest with an access point for each
// published port, its state and level, a manual route's address as a small
// card, or the guests of a path folded into one card ("38 guests on
// vmbr0"), which opens them.
export const TargetCard = memo(function TargetCard({ node, box, tab, lit, ring, selected, still }: CardProps) {
  const { card, part } = litOf(lit)
  const rings = ringOf(ring)
  const address = node.id.startsWith('address:')
  const group = node.kind === 'group'
  const look = routeLook(node.state ?? 'active')
  return (
    <div className={cardClass(group ? 'group' : 'target', node, card, selected)} style={place(box)} data-node={node.id} {...focusable(node.id, tab, nodeLabel(node))}>
      <div className="fm-head">
        <Mark tone={group ? look.tone : 'idle'}>{address ? <RouteIcon /> : <GuestIcon />}</Mark>
        <span className="fm-title">
          <Untrusted text={node.label} />
        </span>
        {node.lines?.[0] && (
          <span className="fm-meta mono">
            <Untrusted text={node.lines[0]} max={14} />
          </span>
        )}
        {rings.has(node.id) && <Ring key={rings.key} />}
      </div>
      {(node.ports ?? []).map((p) => (
        <Port key={p.id} port={p} dim={part(p.id)} ring={rings.has(p.id) ? rings.key : undefined} still={still} />
      ))}
    </div>
  )
}, sameCard)
