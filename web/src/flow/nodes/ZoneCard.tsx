import { memo } from 'react'

import { Badge } from '../../components/Badge'
import { GlobeIcon, ListIcon, WaitIcon } from '../../components/icons'
import { type Look, routeLook } from '../../components/StateBadge'
import { Untrusted } from '../../components/Untrusted'
import { cardWords, nodeLabel, rowLabel } from '../../text/flow'
import { rowItemId } from '../keys'
import type { FlowRow } from '../types'
import { cardClass, type CardProps, focusable, litOf, Mark, place, Ring, ringOf, sameCard } from './parts'

const waits: Look = { tone: 'info', Icon: WaitIcon }
const more: Look = { tone: 'idle', Icon: ListIcon }

function lookOf(r: FlowRow): Look {
  if (r.kind === 'unapproved') return waits
  if (r.kind === 'more' && r.state === 'more') return more
  return routeLook(r.state)
}

function Tag({ tag }: { tag: string }) {
  if (tag === 'waits for approval') return <Badge tone="info">{cardWords.approval}</Badge>
  return <Badge tone={tag === 'DNS' ? 'fail' : 'idle'}>{tag}</Badge>
}

interface RowProps {
  row: FlowRow
  tab?: string
  dim: boolean
  ring?: string
  port?: string
}

function Row({ row, tab, dim, ring, port }: RowProps) {
  const id = rowItemId(row)
  const look = lookOf(row)
  const folded = row.kind === 'more' || row.kind === 'group'
  return (
    <div className={dim ? 'fm-row fm-dim' : 'fm-row'} data-state={row.state} data-kind={row.kind ?? 'route'} {...focusable(id, tab, rowLabel(row, port))}>
      <Mark tone={look.tone}>
        <look.Icon />
      </Mark>
      <span className="fm-host">{folded ? <Untrusted text={row.label ?? ''} /> : <Untrusted text={row.hostname} hostname max={34} />}</span>
      {row.tags.map((t) => (
        <Tag key={t} tag={t} />
      ))}
      {!folded && (
        <span className="fm-owner">
          {row.holder ? (
            <>
              {cardWords.heldBy} <Untrusted text={row.holder} max={16} />
            </>
          ) : (
            <Untrusted text={row.guest ?? row.owner} max={16} />
          )}
        </span>
      )}
      {ring && <Ring key={ring} />}
    </div>
  )
}

function Counts({ counts }: { counts: Record<string, number> }) {
  return (
    <div className="fm-counts">
      {Object.entries(counts).map(([state, n]) => {
        const look = routeLook(state)
        return (
          <span key={state} className="fm-count">
            <Mark tone={look.tone}>
              <look.Icon />
            </Mark>
            {n} <Untrusted text={state} />
          </span>
        )
      })}
    </div>
  )
}

// ZoneCard is a zone with a line for each hostname in it: its state's icon,
// the name, its tags and who asks for it. A folded card ends in a line that
// opens the rest; a collapsed one counts its routes by state. The card of
// the hostnames in no zone, and the card the zones fold into when there are
// too many, are drawn the same way.
export const ZoneCard = memo(function ZoneCard({ node, box, tab, lit, ring, selected, ports }: CardProps) {
  const { card, part } = litOf(lit)
  const rings = ringOf(ring)
  const portOf = ports?.split('\n')
  const named = node.kind === 'zone' && node.ref !== undefined
  const meta = [node.state, ...(node.lines ?? [])].filter((s): s is string => !!s).join(' · ')
  return (
    <div className={cardClass('zone', node, card, selected)} style={place(box)} data-node={node.id}>
      <div className="fm-head" {...focusable(node.id, tab, nodeLabel(node))}>
        <Mark tone={named ? 'info' : 'idle'}>
          <GlobeIcon />
        </Mark>
        <span className="fm-title">{named ? <Untrusted text={node.label} hostname max={30} /> : <Untrusted text={node.label} />}</span>
        {meta && (
          <span className="fm-meta">
            <Untrusted text={meta} max={24} />
          </span>
        )}
        {rings.has(node.id) && <Ring key={rings.key} />}
      </div>
      {node.counts && <Counts counts={node.counts} />}
      {(node.rows ?? []).map((row, at) => {
        const id = rowItemId(row)
        return <Row key={id} row={row} tab={tab} dim={part(id)} ring={rings.has(id) ? rings.key : undefined} port={portOf?.[at] || undefined} />
      })}
    </div>
  )
}, sameCard)
