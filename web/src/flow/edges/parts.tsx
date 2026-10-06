import type { FlowEdge, Point } from '../types'
import { angleAt, pointAt } from './path'

// The dash of each line style: colour never says a state alone, and the
// dashes keep their meaning in forced colours.
export const dashes: Readonly<Partial<Record<FlowEdge['style'], string>>> = {
  unreachable: '6 4',
  withdrawn: '2 3',
  rogue: '4 3',
}

// moving says whether an edge carries dots: a measured figure that is not
// stale, on a line that may move. Hostname lines and the lines of
// connectors pco does not run never do.
export const moving = (e: FlowEdge): boolean => e.style !== 'hairline' && e.style !== 'rogue' && !e.stale && (e.rate ?? 0) > 0

export interface EdgeProps {
  edge: FlowEdge
  from: Point
  to: Point
  dim?: boolean
  // The dots stand still: chevrons and the figure show what they would.
  still?: boolean
}

const samePoint = (a: Point, b: Point) => a.x === b.x && a.y === b.y

export function sameEdge(a: EdgeProps, b: EdgeProps): boolean {
  return a.edge === b.edge && samePoint(a.from, b.from) && samePoint(a.to, b.to) && a.dim === b.dim && a.still === b.still
}

export function edgeClass(e: FlowEdge, dim: boolean | undefined, ...more: string[]): string {
  return ['fm-edge', `fm-edge-${e.style}`, e.muted && 'fm-muted', dim && 'fm-dim', ...more].filter(Boolean).join(' ')
}

const chevronAt = [0.3, 0.5, 0.7]

// Chevrons are the still form of the dots: three arrowheads along the line,
// and the figure beside them where there is room for it.
export function Chevrons({ from, to, figure, dy = 0 }: { from: Point; to: Point; figure?: string; dy?: number }) {
  const at = pointAt(from, to)
  const mid = at(0.5)
  return (
    <g className="fm-chevrons">
      {chevronAt.map((t) => {
        const p = at(t)
        return <path key={t} d="M-3 -4l4 4l-4 4" transform={`translate(${p.x.toFixed(1)} ${(p.y + dy).toFixed(1)}) rotate(${angleAt(from, to, t).toFixed(1)})`} />
      })}
      {figure && (
        <text className="fm-figure" x={mid.x} y={mid.y + dy - 9} textAnchor="middle">
          {figure}
        </text>
      )}
    </g>
  )
}
