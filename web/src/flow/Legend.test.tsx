import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, test } from 'vitest'

import { Legend } from './Legend'

const legend = () => screen.getByRole('list', { name: 'Legend' })
const items = () => within(legend()).getAllByRole('listitem').map((li) => li.textContent)

describe('the legend', () => {
  test('the dots and where their figures come from, the line styles and what moves', () => {
    render(<Legend />)
    expect(items()).toEqual([
      'Requests and proxy errors, from the connector’s own counters',
      'Connections the connector opened to a target, from the egress filter’s counters',
      'Served',
      'Unreachable',
      'Withdrawn (503, DNS kept)',
      'Connector pco does not run',
      'Greyed: not checked, frozen or no new data',
      'Only lines with a measured figure move: dot density follows the rate, errors are red diamonds. Hostname lines never move.',
    ])
    const dashes = [...legend().querySelectorAll('path')].map((p) => p.getAttribute('stroke-dasharray'))
    expect(dashes).toEqual([null, '6 4', '2 3', '4 3', null])
  })

  test.each([
    'the egress filter is off: pco has no per-guest counters',
    'the egress filter is not loaded',
    'the egress filter is not the one pco loads',
  ])('without figures per target it says why: %s', (why) => {
    render(<Legend why={why} />)
    expect(items()[1]).toBe(`Port lines show state only: ${why}`)
    expect(items()).not.toContain('Connections the connector opened to a target, from the egress filter’s counters')
  })

  test('a failed read says so, and what went wrong in its tooltip', () => {
    render(<Legend why="the counters could not be read: nft: signal: killed" />)
    const li = within(legend()).getAllByRole('listitem')[1]
    expect(li?.textContent?.startsWith('Port lines show state only: the counters could not be read')).toBe(true)
    expect(li?.querySelector('bdi')?.textContent).toBe('the counters could not be read')
    const why = screen.getByRole('button', { name: 'What went wrong' })
    fireEvent.focus(why)
    expect(screen.getByRole('tooltip').textContent).toBe('nft: signal: killed')
  })

  test('what moves, paused, under reduced motion and without new data', () => {
    const { rerender } = render(<Legend paused />)
    expect(items().at(-1)).toBe('Motion is paused: chevrons and the figure stand for the dots.')
    rerender(<Legend reduced />)
    expect(items().at(-1)).toBe('Reduced motion: chevrons and the figure stand for the dots.')
    rerender(<Legend stale paused />)
    expect(items().at(-1)).toBe('No new data: the figures are the last ones read, and nothing moves.')
  })
})
