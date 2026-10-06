import { memo } from 'react'

import { figure as num, noDataSince } from '../../text/flow'
import { Chevrons, edgeClass, type EdgeProps, moving, sameEdge } from './parts'
import { curve, laneOffset, pointAt } from './path'

interface TrunkProps extends EdgeProps {
  since?: string // the time of the last sample, when it is stale
}

// Trunk is the line between a tunnel's edge and its connector: a wide pipe
// at low opacity with a lane for each connection to the edge, up to four,
// and the connector's own figures over it. Its dots run in the lanes.
export const Trunk = memo(
  function Trunk({ edge, from, to, dim, still, since }: TrunkProps) {
    const lanes = Math.max(1, Math.min(4, edge.lanes ?? 1))
    const mid = pointAt(from, to)(0.5)
    const figure = edge.rate === undefined ? '' : `${num(edge.rate)} req/s`
    const errors = (edge.errors ?? 0) > 0 ? `${num(edge.errors ?? 0)} errors/s` : ''
    const chevrons = still && moving(edge)
    return (
      <g className={edgeClass(edge, dim, 'fm-pick')} data-edge={edge.id}>
        <path className="fm-pipe" d={curve(from, to)} />
        {Array.from({ length: lanes }, (_, at) => (
          <path key={at} className="fm-lane" d={curve(from, to, laneOffset(at, lanes))} />
        ))}
        {chevrons ? (
          <Chevrons from={from} to={to} figure={figure} />
        ) : (
          figure && (
            <text className="fm-figure" x={mid.x} y={mid.y - 9} textAnchor="middle">
              {figure}
            </text>
          )
        )}
        {errors && (
          <text className="fm-figure fm-errors" x={mid.x} y={mid.y + 18} textAnchor="middle">
            {errors}
          </text>
        )}
        {edge.stale && since && (
          <text className="fm-since" x={mid.x} y={mid.y + (errors ? 32 : 20)} textAnchor="middle">
            {noDataSince} {since}
          </text>
        )}
      </g>
    )
  },
  (a, b) => sameEdge(a, b) && a.since === b.since,
)
