import { describe, expect, test } from 'vitest'

import type { State, TrafficView } from '../api/types.gen'
import { collapse } from './collapse'
import { buildNav, type Nav, step } from './keys'
import { layout } from './layout'
import { buildModel } from './model'
import chains from './testdata/chains.json'
import traffic from './testdata/traffic.json'

const model = collapse(buildModel(chains as unknown as State, traffic as unknown as TrafficView), { expanded: new Set() }).model
const nav = buildNav(model, layout(model))

// walk presses the keys from an item and lists where the focus goes.
function walk(n: Nav, from: string, keys: string[]): string[] {
  const out: string[] = []
  let at = from
  let chain: string | undefined
  for (const key of keys) {
    const moved = step(n, at, key, chain)
    if (!moved) {
      out.push('-')
      continue
    }
    at = moved.id
    chain = moved.chain
    out.push(at)
  }
  return out
}

describe('the keys of the map', () => {
  test('every card, head of a zone card and line of one is an item, column by column', () => {
    const ids = nav.items.map((i) => i.id)
    expect(ids).toContain('zone:example.com')
    expect(ids).toContain('route:www.example.com qemu/101')
    expect(ids).toContain('route:dns.example.com lxc/202')
    expect(ids).toContain('rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807')
    expect(ids).toContain('guest:qemu/101')
    expect([...(nav.bands.get('connector') ?? [])].map((i) => i.id)).toEqual(['connector:acc1', 'rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807', 'connector:acc4'])
  })

  test('Right follows the chain of a hostname to its target, and Left back to its line', () => {
    expect(walk(nav, 'route:www.example.com qemu/101', ['ArrowRight', 'ArrowRight', 'ArrowRight', 'ArrowRight', 'ArrowRight'])).toEqual([
      'edge:acc1',
      'connector:acc1',
      'path:vmbr0.20',
      'guest:qemu/101',
      '-',
    ])
    expect(walk(nav, 'route:api.example.com qemu/103', ['ArrowRight', 'ArrowRight', 'ArrowRight', 'ArrowRight', 'ArrowLeft', 'ArrowLeft', 'ArrowLeft', 'ArrowLeft'])).toEqual([
      'edge:acc1',
      'connector:acc1',
      'path:vmbr0',
      'guest:qemu/103',
      'path:vmbr0',
      'connector:acc1',
      'edge:acc1',
      'route:api.example.com qemu/103',
    ])
  })

  test('Home and End go to the first and the last column, along the chain', () => {
    expect(walk(nav, 'route:www.example.com qemu/101', ['End', 'Home'])).toEqual(['guest:qemu/101', 'route:www.example.com qemu/101'])
    expect(walk(nav, 'connector:acc4', ['End'])).toEqual(['guest:qemu/108'])
  })

  test('Up and Down go through a column in order', () => {
    const list = [...(nav.bands.get('hostnames') ?? [])].map((i) => i.id)
    const at = list.indexOf('zone:example.com')
    expect(walk(nav, 'zone:example.com', ['ArrowDown', 'ArrowDown', 'ArrowUp'])).toEqual([list[at + 1], list[at + 2], list[at + 1]])
    expect(step(nav, list[0] ?? '', 'ArrowUp')?.id).toBe(list[0])
  })

  test('a line without edges still reaches the next column', () => {
    const moved = step(nav, 'route:example.com qemu/105', 'ArrowRight')
    expect(moved?.id.startsWith('edge:')).toBe(true)
  })

  test('a key that is not the map’s moves nothing', () => {
    expect(step(nav, 'edge:acc1', 'a')).toBeUndefined()
    expect(step(nav, 'edge:acc1', 'Tab')).toBeUndefined()
  })
})
