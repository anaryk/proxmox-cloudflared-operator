import { describe, expect, test } from 'vitest'

import type { State, TrafficView } from '../api/types.gen'
import { buildModel } from './model'
import { createMotion, crossing, dotCap, dotsPerSecond, errorShare, type MotionLoop, type MotionOptions } from './motion'
import chains from './testdata/chains.json'
import traffic from './testdata/traffic.json'
import type { FlowEdge, MotionEdge } from './types'

// A clock of frames 1/60 s apart that the test runs by hand.
function frames() {
  let now = 0
  let next: ((t: number) => void) | undefined
  let id = 0
  return {
    frame: (cb: (t: number) => void) => {
      next = cb
      return ++id
    },
    cancel: () => {
      next = undefined
    },
    pending: () => next !== undefined,
    // run moves time on by seconds, a frame at a time, and gives the most
    // dots there were after any frame.
    run(seconds: number, motion: MotionLoop): number {
      let most = 0
      for (let i = 0; i < Math.round(seconds * 60); i++) {
        now += 1000 / 60
        const cb = next
        next = undefined
        cb?.(now)
        most = Math.max(most, motion.dots())
      }
      return most
    },
  }
}

function layer(): SVGGElement {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg')
  const g = document.createElementNS('http://www.w3.org/2000/svg', 'g')
  svg.append(g)
  document.body.append(svg)
  return g
}

const line = (edge: FlowEdge): MotionEdge => ({ edge, at: (t) => ({ x: 100 * t, y: 0 }) })
const port = (id: string, rate: number, more: Partial<FlowEdge> = {}): MotionEdge => line({ id, from: 'p', to: 't', style: 'plain', rate, ...more })
const trunk = (rate: number, more: Partial<FlowEdge> = {}): MotionEdge => line({ id: 'trunk', from: 'e', to: 'c', style: 'trunk', lanes: 4, rate, ...more })

function setup(edges: MotionEdge[], opts: Partial<MotionOptions> = {}) {
  const clock = frames()
  const motion = createMotion({ cap: dotCap, frame: clock.frame, cancel: clock.cancel, hidden: () => false, reducedMotion: () => false, ...opts })
  motion.attach(layer())
  motion.setEdges(edges)
  return { clock, motion }
}

test.each([
  [0, 0],
  [-3, 0],
  [1, 2.704],
  [38, 7.864],
  [1000, 13.5],
  [1e6, 14],
])('%d per second gives %d dots a second', (rate, dots) => {
  expect(dotsPerSecond(rate)).toBeCloseTo(dots, 2)
})

test.each([
  [{ rate: 100, errors: 0 }, 0],
  [{ rate: 100, errors: 1 }, 0.05],
  [{ rate: 100, errors: 50 }, 0.5],
  [{ rate: 0, errors: 1 }, 1],
  [{ rate: 100 }, 0],
])('the share of red diamonds of %o is %d', (edge, share) => {
  expect(errorShare(edge)).toBeCloseTo(share, 5)
})

describe('the loop', () => {
  test('an edge has as many dots as cross it at its rate, each crossing in 1.6 s', () => {
    const { clock, motion } = setup([port('a', 1000)])
    clock.run(10, motion)
    expect(motion.dotsOn('a')).toBeGreaterThanOrEqual(Math.floor(dotsPerSecond(1000) * crossing) - 1)
    expect(motion.dotsOn('a')).toBeLessThanOrEqual(Math.ceil(dotsPerSecond(1000) * crossing) + 1)
  })

  test('the dots are drawn into the layer, in the lanes of the trunk', () => {
    const g = layer()
    const clock = frames()
    const motion = createMotion({ cap: dotCap, frame: clock.frame, cancel: clock.cancel, hidden: () => false, reducedMotion: () => false })
    motion.attach(g)
    motion.setEdges([trunk(1000)])
    clock.run(2, motion)
    const ys = new Set([...g.querySelectorAll('circle:not([visibility])')].map((c) => /translate\([\d.]+ (-?[\d.]+)\)/.exec(c.getAttribute('transform') ?? '')?.[1]))
    expect([...ys].sort()).toEqual(['-1.5', '-4.5', '1.5', '4.5'])
  })

  test('the cap holds with 400 busy edges, the trunk first, then the busiest', () => {
    const edges = [trunk(0.5), ...Array.from({ length: 400 }, (_, i) => port(`p${String(i).padStart(3, '0')}`, 10 + i))]
    const { clock, motion } = setup(edges)
    const most = clock.run(5, motion)
    expect(most).toBeLessThanOrEqual(dotCap)
    // the room is shared out, not left unused
    expect(motion.dots()).toBeGreaterThan(dotCap * 0.8)
    expect(motion.dotsOn('trunk')).toBeGreaterThan(0)
    expect(motion.dotsOn('p399')).toBeGreaterThan(0)
    expect(motion.dotsOn('p000')).toBe(0)
  })

  test('a share of the trunk is red diamonds, at least one in twenty', () => {
    const { clock, motion } = setup([trunk(100, { errors: 1 })])
    let dots = 0
    let errors = 0
    for (let i = 0; i < 40; i++) {
      clock.run(crossing, motion)
      dots += motion.dotsOn('trunk')
      errors += motion.errorsOn('trunk')
    }
    expect(errors / dots).toBeGreaterThan(0.03)
    expect(errors / dots).toBeLessThan(0.08)
  })

  test('hostname edges and rogue connectors never move, whatever they say', () => {
    const { clock, motion } = setup([line({ id: 'h', from: 'z', to: 'e', style: 'hairline', rate: 50 }), line({ id: 'r', from: 'e', to: 'x', style: 'rogue', rate: 50 })])
    clock.run(3, motion)
    expect(motion.dots()).toBe(0)
    expect(clock.pending()).toBe(false)
  })
})

describe('what stops the dots', () => {
  test('a stale figure', () => {
    const { clock, motion } = setup([trunk(100, { stale: true }), port('a', 20)])
    clock.run(3, motion)
    expect(motion.dotsOn('trunk')).toBe(0)
    expect(motion.dotsOn('a')).toBeGreaterThan(0)
  })

  test('the pause, and nothing runs while paused', () => {
    const { clock, motion } = setup([trunk(100)])
    clock.run(2, motion)
    expect(motion.dots()).toBeGreaterThan(0)
    motion.pause()
    expect(motion.dots()).toBe(0)
    expect(clock.pending()).toBe(false)
    clock.run(2, motion)
    expect(motion.dots()).toBe(0)
    motion.resume()
    clock.run(2, motion)
    expect(motion.dots()).toBeGreaterThan(0)
  })

  test('reduced motion', () => {
    let reduced = true
    const { clock, motion } = setup([trunk(100)], { reducedMotion: () => reduced })
    clock.run(2, motion)
    expect(motion.dots()).toBe(0)
    expect(clock.pending()).toBe(false)
    reduced = false
    motion.resume()
    clock.run(2, motion)
    expect(motion.dots()).toBeGreaterThan(0)
  })

  test('a hidden page', () => {
    let hidden = false
    const { clock, motion } = setup([trunk(100)], { hidden: () => hidden })
    clock.run(2, motion)
    expect(motion.dots()).toBeGreaterThan(0)
    hidden = true
    document.dispatchEvent(new Event('visibilitychange'))
    expect(motion.dots()).toBe(0)
    expect(clock.pending()).toBe(false)
    hidden = false
    document.dispatchEvent(new Event('visibilitychange'))
    clock.run(2, motion)
    expect(motion.dots()).toBeGreaterThan(0)
    motion.stop()
  })

  test('an edge outside the viewport', () => {
    const { clock, motion } = setup([port('a', 100), port('b', 100)])
    motion.setVisible(new Set(['a']))
    clock.run(2, motion)
    expect(motion.dotsOn('a')).toBeGreaterThan(0)
    expect(motion.dotsOn('b')).toBe(0)
  })

  test('without the egress counters the edges past the connector stop and the trunk goes on', () => {
    const tv = traffic as unknown as TrafficView
    const counted = buildModel(chains as unknown as State, tv)
    const off = buildModel(chains as unknown as State, { ...tv, routes: [], routesWhy: 'the egress filter is off: pco has no per-guest counters' })
    const run = (edges: FlowEdge[]) => {
      const { clock, motion } = setup(edges.map(line))
      clock.run(3, motion)
      return motion
    }
    const before = run(counted.edges)
    expect(before.dotsOn('edge:acc1>connector:acc1')).toBeGreaterThan(0)
    expect(before.dotsOn('path:vmbr0.20>guest:qemu/101|10.0.0.11:8080')).toBeGreaterThan(0)
    // the lab tunnel's figures are stale
    expect(before.dotsOn('edge:acc4>connector:acc4')).toBe(0)
    const after = run(off.edges)
    expect(after.dotsOn('edge:acc1>connector:acc1')).toBeGreaterThan(0)
    expect(after.dots()).toBe(after.dotsOn('edge:acc1>connector:acc1'))
  })

  test('an edge that went takes its dots with it', () => {
    const { clock, motion } = setup([port('a', 100), port('b', 100)])
    clock.run(2, motion)
    motion.setEdges([port('a', 100)])
    expect(motion.dotsOn('b')).toBe(0)
    expect(motion.dotsOn('a')).toBeGreaterThan(0)
  })

  test('edges that come and go leave nothing behind', () => {
    const { clock, motion } = setup([trunk(100, { errors: 3 })])
    for (let round = 0; round < 50; round++) {
      motion.setEdges([trunk(100, { errors: 3 }), port(`port-${round}`, 50, { errors: 1 })])
      clock.run(0.5, motion)
    }
    expect(motion.remembered()).toBe(2)
    motion.setEdges([trunk(100)])
    expect(motion.remembered()).toBe(1)
  })

  test('stop ends the loop for good', () => {
    const { clock, motion } = setup([trunk(100)])
    clock.run(1, motion)
    motion.stop()
    expect(motion.dots()).toBe(0)
    expect(clock.pending()).toBe(false)
    motion.resume()
    clock.run(1, motion)
    expect(motion.dots()).toBe(0)
  })
})
