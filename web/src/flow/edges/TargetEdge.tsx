import { memo } from 'react'

import type { Point } from '../types'
import { Chevrons, dashes, edgeClass, type EdgeProps, moving, sameEdge } from './parts'
import { curve } from './path'

// The end of a line at its access point says the state again: an arrow
// where the target answers, a cross where it does not, "503" where the rule
// answers for it.
function End({ style, at }: { style: string; at: Point }) {
  const x = at.x - 1
  if (style === 'unreachable') return <path className="fm-end fm-cross" d={`M${x - 8} ${at.y - 4}l7 8m0 -8l-7 8`} />
  if (style === 'withdrawn') {
    return (
      <text className="fm-end fm-503" x={x - 4} y={at.y - 5} textAnchor="end">
        503
      </text>
    )
  }
  return <path className="fm-end fm-arrow" d={`M${x - 6} ${at.y - 3.5}L${x} ${at.y}L${x - 6} ${at.y + 3.5}z`} />
}

// TargetEdge is a line from a path to the access point of a target, in the
// style of its port's worst state, with the connections the connector opens
// to that target as its dots. The pointer finds it on a wider stroke that
// is not drawn.
export const TargetEdge = memo(function TargetEdge({ edge, from, to, dim, still }: EdgeProps) {
  const d = curve(from, to)
  return (
    <g className={edgeClass(edge, dim, 'fm-pick')} data-edge={edge.id}>
      <path className="fm-hit" d={d} />
      <path className="fm-line" d={d} strokeDasharray={dashes[edge.style]} />
      <End style={edge.style} at={to} />
      {still && moving(edge) && <Chevrons from={from} to={to} />}
    </g>
  )
}, sameEdge)
