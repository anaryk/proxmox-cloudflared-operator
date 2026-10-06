import { expect, test } from 'vitest'

import { compareOwners, legOf, legState, routeKey } from './routes'

test('owners in the order of model.CompareOwners', () => {
  const owners = ['manual/status', 'lxc/20', 'qemu/101', 'manual/api', 'qemu/9', 'lxc/3', 'other', 'qemu/0101', 'qemu/2147483648', 'lxc/0']
  expect([...owners].sort(compareOwners)).toEqual([
    'qemu/9',
    'qemu/101',
    'lxc/3',
    'lxc/20',
    'manual/api',
    'manual/status',
    // not guests: a VMID that is not canonical, too large or zero
    'lxc/0',
    'other',
    'qemu/0101',
    'qemu/2147483648',
  ])
})

test('the key of a route is what the address of the Overview writes after route:', () => {
  expect(routeKey({ hostname: 'www.example.com', owner: 'qemu/101' })).toBe('www.example.com qemu/101')
})

test('a path goes through a leg unless it is direct or says nothing', () => {
  expect(legOf({ via: 'attach:vmbr2.20' })).toEqual({ kind: 'attach', network: 'vmbr2.20' })
  expect(legOf({ via: 'managed' })).toEqual({ kind: 'managed' })
  expect(legOf({ via: 'direct' })).toBeUndefined()
  expect(legOf({})).toBeUndefined()
  expect(legOf(undefined)).toBeUndefined()
})

test('the leg of a path is up with a service address, or the diagnosis says what is wrong', () => {
  expect(legState({ addr: '10.50.0.10', service: '198.18.1.5', leg: 'up' })).toEqual({ ok: true, text: 'leg up, service 198.18.1.5 → 10.50.0.10' })
  expect(legState({ service: '198.18.1.5', leg: 'up' })).toEqual({ ok: true, text: 'leg up, service 198.18.1.5' })
  expect(legState({ addr: '10.50.0.10', service: '198.18.1.5', leg: 'the bridge is absent' })).toEqual({ ok: false, text: 'the leg is not up: the bridge is absent' })
  expect(legState({ addr: '10.50.0.10', service: '198.18.1.5' })).toEqual({ ok: false, text: 'the leg is not up: the gateway has not said how it is' })
  expect(legState({ addr: '10.50.0.10', leg: 'up' })).toEqual({ ok: false, text: 'no service address' })
})
