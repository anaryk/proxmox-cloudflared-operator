import { memo } from 'react'

import { GlobeIcon } from '../../components/icons'
import { Untrusted } from '../../components/Untrusted'
import { nodeLabel } from '../../text/flow'
import { cardClass, type CardProps, focusable, Lines, litOf, Mark, place, Ring, ringOf, sameCard, verifiedTone } from './parts'

// EdgeNode is a tunnel where it meets Cloudflare's edge: the tunnel by its
// account and the start of its id, whether the last cycle verified it, its
// connections to the edge, where they land and their round trip.
export const EdgeNode = memo(function EdgeNode({ node, box, tab, lit, ring, selected }: CardProps) {
  const rings = ringOf(ring)
  const lines = [...(node.state ? [`verified: ${node.state}`] : []), ...(node.lines ?? [])]
  return (
    <div className={cardClass('edge', node, litOf(lit).card, selected)} style={place(box)} data-node={node.id} {...focusable(node.id, tab, nodeLabel(node))}>
      <div className="fm-head">
        <Mark tone={verifiedTone(node.state)}>
          <GlobeIcon />
        </Mark>
        <span className="fm-title">
          <Untrusted text={node.label} />
        </span>
      </div>
      <Lines lines={lines.map((l, at) => <Untrusted key={at} text={l} />)} />
      {rings.has(node.id) && <Ring key={rings.key} />}
    </div>
  )
}, sameCard)
