import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { type Sample, TrafficChart } from './TrafficChart'

const end = '2026-10-05T10:15:00Z'

// A sample every 5 s over the 15 minutes before end, value(i) for the i-th.
function samples(value: (i: number) => number, skip: (i: number) => boolean = () => false): Sample[] {
  const out: Sample[] = []
  for (let i = 0; i < 180; i++) {
    if (!skip(i)) out.push({ at: new Date(Date.parse(end) - (179 - i) * 5000).toISOString(), value: value(i) })
  }
  return out
}

beforeEach(() => {
  vi.stubEnv('TZ', 'UTC')
})

afterEach(() => {
  vi.unstubAllEnvs()
  vi.restoreAllMocks()
})

describe('TrafficChart', () => {
  test('one series: a line, no legend, the latest values and the summary in words', () => {
    const { container } = render(
      <TrafficChart label="Connections opened to 10.0.0.11:8080" end={end} series={[{ name: 'connections opened', unit: 'flows/s', samples: samples((i) => i / 10) }]} />,
    )
    expect(screen.getByRole('img', { name: 'Connections opened to 10.0.0.11:8080' })).toBeTruthy()
    expect(container.querySelectorAll('polyline')).toHaveLength(1)
    expect(container.querySelector('.chart-legend')).toBeNull()
    expect(container.querySelector('.chart-readout')?.textContent).toBe('10:15:00 +00:00 · connections opened 17.9 flows/s')
    expect(container.querySelector('.sr-only')?.textContent).toBe('connections opened: 17.9 flows/s now, at most 17.9, at least 0.0')
    expect(container.querySelector('.chart-foot')?.textContent).toContain('up to 20.0 flows/s')
  })

  test('two series: a legend, the second line drawn apart', () => {
    const { container } = render(
      <TrafficChart
        label="Requests of the tunnel"
        end={end}
        series={[
          { name: 'requests', unit: 'req/s', samples: samples(() => 38.2) },
          { name: 'errors', unit: 'errors/s', samples: samples(() => 0.2) },
        ]}
      />,
    )
    expect(container.querySelector('.chart-legend')?.textContent).toBe('requestserrors')
    expect(container.querySelectorAll('polyline.chart-second')).toHaveLength(1)
    expect(container.querySelector('.chart-readout')?.textContent).toBe('10:15:00 +00:00 · requests 38.2 req/s · errors 0.2 errors/s')
  })

  test('a missing sample breaks the line', () => {
    const { container } = render(
      <TrafficChart label="Requests" end={end} series={[{ name: 'requests', unit: 'req/s', samples: samples(() => 1, (i) => i === 90) }]} />,
    )
    expect(container.querySelectorAll('polyline')).toHaveLength(2)
  })

  test('samples older than 15 minutes are left out', () => {
    const old = [{ at: '2026-10-05T09:59:55Z', value: 1000 }, ...samples(() => 1)]
    const { container } = render(<TrafficChart label="Requests" end={end} series={[{ name: 'requests', unit: 'req/s', samples: old }]} />)
    expect(container.querySelector('.sr-only')?.textContent).toBe('requests: 1.0 req/s now, at most 1.0, at least 1.0')
  })

  test('the values under the pointer', () => {
    const { container } = render(
      <TrafficChart label="Requests" end={end} series={[{ name: 'requests', unit: 'req/s', samples: samples((i) => i) }]} />,
    )
    const svg = screen.getByRole('img', { name: 'Requests' })
    vi.spyOn(svg, 'getBoundingClientRect').mockReturnValue({ left: 0, width: 300, top: 0, height: 60, right: 300, bottom: 60, x: 0, y: 0, toJSON: () => ({}) })
    // halfway is 10:07:30, the sample of value 89
    fireEvent.pointerMove(svg, { clientX: 150 })
    expect(container.querySelector('.chart-readout')?.textContent).toBe('10:07:30 +00:00 · requests 89.0 req/s')
    expect(container.querySelector('.chart-cursor')).not.toBeNull()
    fireEvent.pointerLeave(svg)
    expect(container.querySelector('.chart-cursor')).toBeNull()
  })

  test('no samples', () => {
    const { container } = render(<TrafficChart label="Requests" end={end} stale series={[{ name: 'requests', unit: 'req/s', samples: [] }]} />)
    expect(container.querySelector('.chart-readout')?.textContent).toBe('no samples')
    expect(container.querySelector('.sr-only')?.textContent).toBe('No new samples. requests: no samples')
    expect(container.querySelector('figure')?.classList.contains('chart-stale')).toBe(true)
  })
})
