import { describe, expect, test, vi } from 'vitest'

import type { State, TrafficView } from '../api/types.gen'
import { isCommand } from '../text/words'
import { buildModel } from './model'
import type { MotionLoop } from './motion'
import { changesOf, extrasOf, keepSame, restartable } from './panel'
import chains from './testdata/chains.json'
import traffic from './testdata/traffic.json'

const st = chains as unknown as State
const tv = traffic as unknown as TrafficView

describe('from one view to the next', () => {
  test('what did not change is the same object, what changed is new', () => {
    const before = buildModel(st, tv)
    const routes = st.routes.map((r) => (r.hostname === 'old.example.com' ? { ...r, state: 'unreachable', reason: 'target is not answering' } : r))
    const next = keepSame(before, buildModel({ ...st, routes }, tv))
    const same = (id: string) => next.nodes.find((n) => n.id === id) === before.nodes.find((n) => n.id === id)
    expect(same('edge:acc1')).toBe(true)
    expect(same('guest:qemu/101')).toBe(true)
    expect(same('zone:example.com')).toBe(false)
    expect(next.edges.find((e) => e.id === 'edge:acc1>connector:acc1')).toBe(before.edges.find((e) => e.id === 'edge:acc1>connector:acc1'))
  })

  test('what changed state rings; what is new or gone does not', () => {
    const before = buildModel(st, tv)
    const routes = st.routes
      .filter((r) => r.hostname !== 'lab.example.dev')
      .map((r) => (r.hostname === 'api.example.com' ? { ...r, state: 'active', reason: undefined, service: 'http://10.0.0.13:9000' } : r))
    const ids = changesOf(before, buildModel({ ...st, routes }, tv))
    expect([...ids].sort()).toEqual(['guest:qemu/103|10.0.0.13:9000', 'route:api.example.com qemu/103'])
  })

  test('the figures of a trunk beside the line, and the rotate command of the connectors that need one', () => {
    const model = buildModel(st, tv)
    const extras = extrasOf(st, tv, model)
    expect(extras.concurrent?.get('edge:acc1>connector:acc1')).toBe(tv.tunnels[0]?.samples.at(-1)?.concurrent)
    expect(extras.since?.has('edge:acc1>connector:acc1')).toBe(false)
    expect(extras.since?.get('edge:acc4>connector:acc4')).toMatch(/^\d\d:\d\d:\d\d$/)
    const rogue = extras.commands?.get('rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807')
    expect(rogue?.refused).toBe('the account id has an unexpected form')
    expect(extras.commands?.has('connector:acc1')).toBe(false)

    const account = '0123456789abcdef0123456789abcdef'
    const named = JSON.parse(JSON.stringify(st).replaceAll('"acc1"', `"${account}"`)) as State
    const refused = { ...named, connectors: named.connectors.map((c, i) => (i === 0 ? { ...c, tokenRefused: true } : c)) }
    const cmd = extrasOf(refused, tv, buildModel(refused, tv)).commands?.get(`connector:${account}`)
    expect(isCommand(cmd?.command) && cmd.command.text).toBe(`pco tunnel rotate --account ${account}`)
  })
})

describe('a motion that starts again', () => {
  test('a new loop gets what the map gave the old one', () => {
    const loops: MotionLoop[] = []
    const make = () => {
      const loop = { attach: vi.fn(), setEdges: vi.fn(), setVisible: vi.fn(), pause: vi.fn(), resume: vi.fn(), stop: vi.fn(), dots: () => 0, dotsOn: () => 0, errorsOn: () => 0, remembered: () => 0 }
      loops.push(loop)
      return loop
    }
    const m = restartable(make)
    const layer = document.createElementNS('http://www.w3.org/2000/svg', 'g')
    m.attach(layer)
    m.setEdges([])
    m.pause()
    m.start()
    m.stop()
    m.start()
    expect(loops).toHaveLength(2)
    expect(loops[0]?.stop).toHaveBeenCalled()
    expect(loops[1]?.attach).toHaveBeenCalledWith(layer)
    expect(loops[1]?.setEdges).toHaveBeenCalledWith([])
    expect(loops[1]?.pause).toHaveBeenCalled()
    m.resume()
    expect(loops[1]?.resume).toHaveBeenCalled()
  })
})
