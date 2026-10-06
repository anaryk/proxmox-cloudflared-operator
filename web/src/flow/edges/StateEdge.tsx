import { memo } from 'react'

import { Chevrons, dashes, edgeClass, type EdgeProps, moving, sameEdge } from './parts'
import { curve } from './path'

// StateEdge is a line that does not end at a target: the bundle of a zone
// card's hostnames to their tunnel ("6 rules", never moving), the line from
// a connector to a path, which carries the connections to every target
// behind it and says their sum in its tooltip, and the red dashed line to a
// connector pco does not run, which never moves either.
export const StateEdge = memo(function StateEdge({ edge, from, to, dim, still }: EdgeProps) {
  const d = curve(from, to)
  const told = edge.style !== 'hairline' && edge.style !== 'rogue'
  return (
    <g className={edgeClass(edge, dim, told ? 'fm-pick' : '')} data-edge={edge.id}>
      {told && <path className="fm-hit" d={d} />}
      <path className="fm-line" d={d} strokeDasharray={dashes[edge.style]} />
      {edge.style === 'hairline' && edge.label && (
        <text className="fm-edge-label" x={from.x + 3} y={from.y - 5}>
          {edge.label}
        </text>
      )}
      {still && moving(edge) && <Chevrons from={from} to={to} />}
    </g>
  )
}, sameEdge)
