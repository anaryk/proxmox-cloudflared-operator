import { memo } from 'react'

import { NodesIcon, RogueIcon } from '../../components/icons'
import type { Tone } from '../../components/icons'
import { Untrusted } from '../../components/Untrusted'
import { cardWords, nodeLabel } from '../../text/flow'
import { cardClass, type CardProps, focusable, Lines, litOf, Mark, place, Ring, ringOf, sameCard } from './parts'

// The tone of a replica by its state: ready, on its way, or gone.
export function replicaTone(state: string | undefined): Tone {
  switch (state) {
    case 'ready':
      return 'ok'
    case 'not ready':
    case 'joining':
    case 'draining':
      return 'warn'
    case '':
    case undefined:
      return 'idle'
  }
  return 'fail'
}

// ReplicaNode is a node of the cluster where it runs the connector of a
// tunnel: the node as a chip coloured by whether that connector is ready,
// the tunnel, and what the node reports of its targets. The replicas of a
// tunnel folded into one card say how many are ready and open on a click.
export const ReplicaNode = memo(function ReplicaNode({ node, box, tab, lit, ring, selected, refused }: CardProps) {
  const rings = ringOf(ring)
  const tone = replicaTone(node.state)
  const folded = node.kind === 'group'
  return (
    <div className={cardClass(folded ? 'group' : 'replica', node, litOf(lit).card, selected, refused && 'fm-refused')} style={place(box)} data-node={node.id} {...focusable(node.id, tab, nodeLabel(node))}>
      <div className="fm-head">
        <Mark tone={tone}>
          <NodesIcon />
        </Mark>
        {folded ? (
          <span className="fm-title">
            <Untrusted text={node.label} />
          </span>
        ) : (
          <span className={`fm-chip status-${tone}`}>
            <Untrusted text={node.label} max={18} />
          </span>
        )}
        {refused && (
          <Mark tone="fail">
            <RogueIcon label={cardWords.refused} />
          </Mark>
        )}
      </div>
      <Lines lines={[...(folded ? [] : [node.state ?? '']), ...(node.lines ?? [])].map((l, at) => <Untrusted key={at} text={l} />)} />
      {rings.has(node.id) && <Ring key={rings.key} />}
    </div>
  )
}, sameCard)
