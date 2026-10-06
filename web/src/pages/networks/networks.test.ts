import { describe, expect, test } from 'vitest'

import type { PathView, RouteView } from '../../api/types.gen'
import { guestsOf, levelsOf, networksOf } from './networks'

const path = (bridge: string | undefined, vlan?: number): PathView => ({ node: 'pve1', bridge, vlan, verifiedAt: '2026-10-01T12:00:00Z' })

const route = (hostname: string, owner: string, patch: Partial<RouteView> = {}): RouteView => ({ hostname, owner, state: 'active', level: 'port', ...patch })

const guest = (vmid: number, name?: string) => ({ kind: 'qemu', vmid, name })

describe('networksOf', () => {
  test('a network for each bridge and VLAN, the untagged first, and the one without a bridge last', () => {
    const { networks } = networksOf([
      route('manual.example.com', 'manual/a', { level: 'manual' }),
      route('c.example.com', 'qemu/103', { path: path('vmbr1', 20) }),
      route('a.example.com', 'qemu/101', { path: path('vmbr1') }),
      route('b.example.com', 'qemu/102', { path: path('vmbr0', 5) }),
      route('d.example.com', 'qemu/104', { path: path('vmbr1', 3) }),
    ])
    expect(networks.map((n) => [n.bridge, n.vlan, n.routes.length])).toEqual([
      ['vmbr0', 5, 1],
      ['vmbr1', undefined, 1],
      ['vmbr1', 3, 1],
      ['vmbr1', 20, 1],
      [undefined, undefined, 1],
    ])
  })

  test('routes on the same bridge and VLAN are one network', () => {
    const { networks } = networksOf([
      route('a.example.com', 'qemu/101', { path: path('vmbr0', 20) }),
      route('b.example.com', 'qemu/102', { path: path('vmbr0', 20) }),
      route('c.example.com', 'qemu/103', { path: path('vmbr0') }),
    ])
    expect(networks.map((n) => n.routes.map((r) => r.hostname))).toEqual([['c.example.com'], ['a.example.com', 'b.example.com']])
  })

  test('a manual route and a proof that placed no MAC on a bridge are on none', () => {
    const { networks, unproven } = networksOf([
      route('manual.example.com', 'manual/a', { level: undefined }),
      route('behind.example.com', 'qemu/101', { level: 'observed', path: path(undefined) }),
      route('router.example.com', 'qemu/102', { level: 'observed' }),
    ])
    expect(networks).toHaveLength(1)
    expect(networks[0]?.bridge).toBeUndefined()
    expect(networks[0]?.routes).toHaveLength(3)
    expect(unproven).toBe(0)
  })

  test('a VLAN without a bridge is not a network', () => {
    const { networks } = networksOf([route('a.example.com', 'qemu/101', { level: 'observed', path: { ...path(undefined, 20) } })])
    expect(networks.map((n) => [n.bridge, n.vlan])).toEqual([[undefined, undefined]])
  })

  test('a route none of whose addresses was proven is on no network, and counted', () => {
    const { networks, unproven } = networksOf([
      route('a.example.com', 'qemu/101', { level: undefined, state: 'conflict' }),
      route('b.example.com', 'qemu/102', { level: undefined, state: 'held' }),
    ])
    expect(networks).toEqual([])
    expect(unproven).toBe(2)
  })
})

test('levels are counted, the strongest first and an unknown one last', () => {
  expect(
    levelsOf([
      route('a', 'qemu/1', { level: 'observed' }),
      route('b', 'qemu/2', { level: 'port' }),
      route('c', 'qemu/3', { level: 'observed' }),
      route('d', 'manual/d', { level: 'manual' }),
      route('e', 'qemu/5', { level: 'newer' }),
      route('f', 'qemu/6', { level: undefined }),
    ]),
  ).toEqual([
    ['port', 1],
    ['observed', 2],
    ['manual', 1],
    ['newer', 1],
    ['none', 1],
  ])
})

test('guests are counted once, by owner', () => {
  expect(
    guestsOf([
      route('a', 'qemu/102', { guest: guest(102, 'web-2') }),
      route('b', 'qemu/101', { guest: guest(101, 'web-1') }),
      route('c', 'qemu/101', { guest: guest(101, 'web-1') }),
      route('d', 'manual/d'),
    ]),
  ).toEqual([
    { ref: 'qemu/101', name: 'web-1' },
    { ref: 'qemu/102', name: 'web-2' },
  ])
})
