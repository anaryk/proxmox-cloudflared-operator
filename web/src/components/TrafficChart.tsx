import { type PointerEvent, useId, useState } from 'react'

import { partsIn } from './Time'

export interface Sample {
  at: string
  value: number
}

export interface Series {
  name: string // "requests", "errors", "connections opened"
  unit: string // "req/s"
  samples: readonly Sample[]
}

const width = 300
const height = 60
const span = 15 * 60_000

interface Point {
  t: number
  value: number
}

type XY = [number, number]

const figure = (v: number) => v.toFixed(1)

const coords = (run: readonly XY[]) => run.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' ')

// top is the value the chart reaches to: 1, 2, 2.5 or 5 times a power of
// ten, at least the largest value.
function top(values: number[]): number {
  const most = Math.max(0, ...values)
  if (most <= 0) return 1
  const power = 10 ** Math.floor(Math.log10(most))
  return ([1, 2, 2.5, 5, 10].find((m) => m * power >= most) ?? 10) * power
}

// runs are the lines of a series. An interval without a sample, such as the
// one in which a counter restarted, breaks the line.
function runs(points: readonly Point[], start: number, interval: number, scale: number): XY[][] {
  const out: XY[][] = []
  let last = -Infinity
  for (const p of points) {
    if (p.t - last > 1.5 * interval) out.push([])
    out[out.length - 1]?.push([((p.t - start) / span) * width, height - 2 - (p.value / scale) * (height - 4)])
    last = p.t
  }
  return out
}

function area(run: readonly XY[]): string {
  const first = run[0]?.[0] ?? 0
  const last = run[run.length - 1]?.[0] ?? 0
  return coords([[first, height], ...run, [last, height]])
}

function nearest(points: readonly Point[], t: number): Point | undefined {
  let best: Point | undefined
  for (const p of points) {
    if (!best || Math.abs(p.t - t) < Math.abs(best.t - t)) best = p
  }
  return best
}

function clock(t: number): string {
  const p = partsIn(new Date(t))
  return `${p.time} ${p.offset}`
}

export interface TrafficChartProps {
  label: string
  series: readonly [Series] | readonly [Series, Series]
  // The time of the daemon's last sample; the chart ends there.
  end: string
  interval?: number
  stale?: boolean
}

// TrafficChart draws one or two series of the last 15 minutes: a tunnel's
// requests and errors, or the connections opened to a target. It reads out
// the values under the pointer, and says what it shows in words as well.
export function TrafficChart({ label, series, end, interval = 5000, stale }: TrafficChartProps) {
  const summaryId = useId()
  const [hover, setHover] = useState<number>()
  const last = Date.parse(end)
  const start = last - span
  const points = series.map((s) =>
    s.samples
      .map((x) => ({ t: Date.parse(x.at), value: x.value }))
      .filter((p) => p.t >= start && p.t <= last && Number.isFinite(p.value))
      .sort((a, b) => a.t - b.t),
  )
  const scale = top(points.flat().map((p) => p.value))
  const main = points[0] ?? []

  const summary = series
    .map((s, at) => {
      const values = (points[at] ?? []).map((p) => p.value)
      const now = values[values.length - 1]
      if (now === undefined) return `${s.name}: no samples`
      return `${s.name}: ${figure(now)} ${s.unit} now, at most ${figure(Math.max(...values))}, at least ${figure(Math.min(...values))}`
    })
    .join('; ')

  const at = hover === undefined ? main[main.length - 1] : nearest(main, hover)
  const readout = at
    ? [
        clock(at.t),
        ...series.map((s, i) => {
          const p = nearest(points[i] ?? [], at.t)
          return p && Math.abs(p.t - at.t) <= interval / 2 ? `${s.name} ${figure(p.value)} ${s.unit}` : `no sample of ${s.name}`
        }),
      ].join(' · ')
    : 'no samples'

  const move = (e: PointerEvent<SVGSVGElement>) => {
    const box = e.currentTarget.getBoundingClientRect()
    if (box.width <= 0) return
    setHover(start + Math.min(Math.max((e.clientX - box.left) / box.width, 0), 1) * span)
  }

  const cursor = hover !== undefined && at ? ((at.t - start) / span) * width : undefined
  return (
    <figure className={stale ? 'chart chart-stale' : 'chart'}>
      <figcaption className="chart-head">
        <span className="chart-title">{label}</span>
        <span className="chart-readout num" aria-hidden="true">
          {readout}
        </span>
      </figcaption>
      <svg
        className="chart-plot"
        viewBox={`0 0 ${width} ${height}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={label}
        aria-describedby={summaryId}
        onPointerMove={move}
        onPointerLeave={() => setHover(undefined)}
      >
        {runs(main, start, interval, scale).map((run) => (
          <polygon key={`area ${run[0]?.[0]}`} className="chart-area" points={area(run)} />
        ))}
        {points.map((ps, i) =>
          runs(ps, start, interval, scale).map((run) => (
            <polyline
              key={`${i} ${run[0]?.[0]}`}
              className={i === 0 ? 'chart-line' : 'chart-line chart-second'}
              points={coords(run)}
              vectorEffect="non-scaling-stroke"
            />
          )),
        )}
        {cursor !== undefined && <line className="chart-cursor" x1={cursor} x2={cursor} y1="0" y2={height} vectorEffect="non-scaling-stroke" />}
      </svg>
      <div className="chart-foot">
        <span className="num">
          up to {figure(scale)} {series[0].unit}, the last 15 minutes
        </span>
        {series.length === 2 && (
          <span className="chart-legend">
            {series.map((s, i) => (
              <span key={s.name} className={i === 0 ? 'chart-key' : 'chart-key chart-key-second'}>
                {s.name}
              </span>
            ))}
          </span>
        )}
      </div>
      <p id={summaryId} className="sr-only">
        {stale ? `No new samples. ${summary}` : summary}
      </p>
    </figure>
  )
}
