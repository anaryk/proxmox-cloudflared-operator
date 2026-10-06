import { memo } from 'react'

import { NetworkIcon } from '../../components/icons'
import { Untrusted } from '../../components/Untrusted'
import { nodeLabel } from '../../text/flow'
import { cardClass, type CardProps, focusable, litOf, Mark, place, sameCard } from './parts'

// PathNode is the bridge and VLAN the targets behind it were proven on, or
// that no bridge was proven: the bridge on top, the VLAN and how the target
// is reached under it. No subnet is drawn: nothing knows one.
export const PathNode = memo(function PathNode({ node, box, tab, lit, selected }: CardProps) {
  const [bridge = node.label, ...rest] = node.ref === 'path:none' ? [node.label] : node.label.split(' · ')
  const sub = rest.join(' · ')
  return (
    <div className={cardClass('path', node, litOf(lit).card, selected)} style={place(box)} data-node={node.id} {...focusable(node.id, tab, nodeLabel(node))}>
      <div className="fm-head">
        <Mark tone="idle">
          <NetworkIcon />
        </Mark>
        <span className={node.ref === 'path:none' ? 'fm-title' : 'fm-title mono'}>
          <Untrusted text={bridge} />
        </span>
      </div>
      {sub && (
        <div className="fm-text fm-sub">
          <Untrusted text={sub} />
        </div>
      )}
    </div>
  )
}, sameCard)
