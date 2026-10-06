// The shape of an edge: the mockup's curve, which leaves its card and
// enters the next horizontally, its control points halfway across. The
// dots, the chevrons and the labels sit on it by the fraction of its length.

import type { Point } from '../types'

export function curve(from: Point, to: Point, dy = 0): string {
  const mid = (from.x + to.x) / 2
  const a = from.y + dy
  const b = to.y + dy
  return `M${from.x} ${a}C${mid} ${a} ${mid} ${b} ${to.x} ${b}`
}

export function pointAt(from: Point, to: Point): (t: number) => Point {
  const mid = (from.x + to.x) / 2
  return (t) => {
    const u = 1 - t
    const a = u * u * u
    const b = 3 * u * u * t
    const c = 3 * u * t * t
    const d = t * t * t
    return { x: a * from.x + (b + c) * mid + d * to.x, y: (a + b) * from.y + (c + d) * to.y }
  }
}

// angleAt is the direction of the curve at t, in degrees.
export function angleAt(from: Point, to: Point, t: number): number {
  const mid = (from.x + to.x) / 2
  const u = 1 - t
  const dx = 3 * u * u * (mid - from.x) + 3 * t * t * (to.x - mid)
  const dy = 6 * u * t * (to.y - from.y)
  return (Math.atan2(dy, dx) * 180) / Math.PI
}

// laneOffset is where the nth of n lanes of the trunk runs, across it; the
// dots of the loop use the same spacing.
export const laneOffset = (at: number, lanes: number): number => (at - (lanes - 1) / 2) * 3
