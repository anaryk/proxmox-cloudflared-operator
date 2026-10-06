import { memo } from 'react'

import { Badge } from '../../components/Badge'
import { RogueIcon, TunnelIcon } from '../../components/icons'
import { Untrusted } from '../../components/Untrusted'
import { nodeLabel } from '../../text/flow'
import { cardClass, type CardProps, connectorTone, focusable, Lines, litOf, Mark, place, Ring, ringOf, sameCard } from './parts'

// ConnectorNode is the connector pco runs for a tunnel on this node: the
// node, connectorText's words, the tunnel, the configuration it runs and
// its cloudflared. Whose token Cloudflare refuses is marked; the command
// that gives the tunnel a new secret is in its tooltip and its drawer.
export const ConnectorNode = memo(function ConnectorNode({ node, box, tab, lit, ring, selected, refused }: CardProps) {
  const rings = ringOf(ring)
  const tone = connectorTone(node.state)
  return (
    <div className={cardClass('connector', node, litOf(lit).card, selected, refused && 'fm-refused')} style={place(box)} data-node={node.id} {...focusable(node.id, tab, nodeLabel(node))}>
      <div className="fm-head">
        <Mark tone={tone}>
          <TunnelIcon />
        </Mark>
        <span className="fm-title">
          <Untrusted text={node.label} />
        </span>
        {refused && (
          <Mark tone="fail">
            <RogueIcon label="Cloudflare refuses its token" />
          </Mark>
        )}
        {node.tags?.map((t) => (
          <Badge key={t} tone="warn">
            {t}
          </Badge>
        ))}
      </div>
      <Lines lines={[node.state ?? '', ...(node.lines ?? [])].map((l, at) => <Untrusted key={at} text={l} />)} />
      {rings.has(node.id) && <Ring key={rings.key} />}
    </div>
  )
}, sameCard)
