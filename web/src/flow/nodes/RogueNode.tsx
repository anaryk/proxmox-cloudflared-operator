import { memo } from 'react'

import { RogueIcon } from '../../components/icons'
import { Untrusted } from '../../components/Untrusted'
import { nodeLabel } from '../../text/flow'
import { cardClass, type CardProps, focusable, Lines, litOf, Mark, place, sameCard } from './parts'

const notRun = 'not run by pco'

// RogueNode is a connector Cloudflare lists on a tunnel that pco does not
// run, in red, beside the connector pco runs: where it connects from and
// its cloudflared. The command that cuts it off is in its tooltip and its
// drawer; the page never runs it.
export const RogueNode = memo(function RogueNode({ node, box, tab, lit, selected }: CardProps) {
  const from = node.label.startsWith(`${notRun}: `) ? node.label.slice(notRun.length + 2) : undefined
  const line = from ?? node.lines?.[0]
  return (
    <div className={cardClass('rogue', node, litOf(lit).card, selected)} style={place(box)} data-node={node.id} {...focusable(node.id, tab, nodeLabel(node))}>
      <div className="fm-head">
        <Mark tone="fail">
          <RogueIcon />
        </Mark>
        <span className="fm-title">{from === undefined ? <Untrusted text={node.label} /> : notRun}</span>
      </div>
      {line && <Lines lines={[<Untrusted key="line" text={line} />]} />}
    </div>
  )
}, sameCard)
