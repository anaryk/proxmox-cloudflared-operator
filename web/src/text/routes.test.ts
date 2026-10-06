import { expect, test } from 'vitest'

import { compareOwners, routeKey } from './routes'

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
