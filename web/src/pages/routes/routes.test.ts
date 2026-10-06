import { describe, expect, test } from 'vitest'

import type { RouteView } from '../../api/types.gen'
import populated from '../../fixtures/populated.json'
import scenario from '../../fixtures/scenario-populated.json'
import { routeStateOrder } from '../../gen/words.gen'
import {
  allowHostLink,
  compareRoutes,
  holderOf,
  matchesRoute,
  noFilter,
  openableURL,
  pathText,
  routeLink,
  stateCounts,
} from './routes'

const routes = scenario.routes as RouteView[]

test('routes by hostname, then owner', () => {
  const sorted = [...routes].sort(compareRoutes).map((r) => `${r.hostname} ${r.owner}`)
  expect(sorted.slice(0, 3)).toEqual(['*.store.example.com lxc/210', 'app.example.com qemu/101', 'db-admin.example.com qemu/106'])
  expect(sorted.slice(-3)).toEqual(['www.example.com qemu/101', 'www.example.com qemu/102', 'www.store.example.com lxc/210'])
})

describe('the filter', () => {
  test('states, zone and text', () => {
    const names = (f: Partial<typeof noFilter>) => routes.filter((r) => matchesRoute(r, { ...noFilter, ...f })).map((r) => r.hostname)
    expect(names({})).toHaveLength(routes.length)
    expect(names({ states: ['no-zone', 'rejected'] })).toEqual(['*.store.example.com', 'media.example.io', 'wiki.example.org'])
    expect(names({ zone: 'example.info' })).toEqual(['notes.example.info'])
    // the guest's name, the owner and the service as well as the hostname
    expect(names({ text: 'WEB-2' })).toEqual(['www.example.com'])
    expect(names({ text: 'lxc/230' })).toEqual(['vpn.example.com'])
    expect(names({ text: '10.0.5.20' })).toEqual(['status.example.com'])
    expect(names({ states: ['active'], text: 'store' })).toEqual(['store.example.com', 'www.store.example.com'])
  })

  test('the counts of every state in the order of present.RouteStateOrder', () => {
    expect(stateCounts(routes, routeStateOrder)).toEqual([
      ['active', 8],
      ['unreachable', 1],
      ['withdrawn', 1],
      ['conflict', 1],
      ['no-zone', 2],
      ['held', 1],
      ['rejected', 1],
      ['frozen', 1],
    ])
    expect(stateCounts([{ ...(routes[0] as RouteView), state: 'newer' }], routeStateOrder).at(-1)).toEqual(['newer', 1])
  })
})

test('the holder of a hostname is the route that did not lose it', () => {
  const st = populated.routes as RouteView[]
  expect(holderOf(st, 'www.example.com')?.owner).toBe('qemu/101')
  expect(holderOf([...st].reverse(), 'WWW.example.com')?.owner).toBe('qemu/101')
  expect(holderOf(st.filter((r) => r.state === 'conflict'), 'www.example.com')?.owner).toBe('qemu/102')
  expect(holderOf(st, 'none.example.com')).toBeUndefined()
})

test('the path: node, bridge, VLAN and port, or no bridge proven', () => {
  expect(pathText({ node: 'pve1', bridge: 'vmbr0', vlan: 20, port: 'tap101i0', verifiedAt: '' })).toBe('pve1 · vmbr0 · VLAN 20 · tap101i0')
  expect(pathText({ node: 'pve1', bridge: 'vmbr1', port: 'veth205i0', verifiedAt: '' })).toBe('pve1 · vmbr1 · veth205i0')
  expect(pathText({ node: 'pve1', verifiedAt: '' })).toBe('pve1 · no bridge proven')
  expect(pathText(undefined)).toBe('no bridge proven')
})

test('a link opens exact names only', () => {
  expect(openableURL('www.example.com')).toBe('https://www.example.com/')
  for (const name of ['*.store.example.com', 'example', 'a.b/c.example.com', 'x.example.com:8080', 'javascript:alert(1)', 'A.example.com']) {
    expect(openableURL(name), name).toBeUndefined()
  }
})

test('the routes and settings links are encoded', () => {
  expect(routeLink('www.example.com', 'qemu/101')).toBe('/routes/www.example.com?owner=qemu%2F101')
  expect(routeLink('*.store.example.com')).toBe('/routes/*.store.example.com')
  const rejected = routes.find((r) => r.state === 'rejected') as RouteView
  expect(allowHostLink(rejected)).toBe('/settings?addAllowHost=*.store.example.com&owner=lxc%2F210')
  expect(allowHostLink({ ...rejected, hostname: '*.x&y=z.example.com', owner: 'lxc/2#1' })).toBe(
    '/settings?addAllowHost=*.x%26y%3Dz.example.com&owner=lxc%2F2%231',
  )
  // never for another state, never for a manual route
  expect(allowHostLink({ ...rejected, state: 'active' })).toBeUndefined()
  expect(allowHostLink({ ...rejected, owner: 'manual/x' })).toBeUndefined()
})
