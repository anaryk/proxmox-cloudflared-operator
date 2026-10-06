import { describe, expect, test } from 'vitest'

import type { ManualRouteView } from '../../api/types.gen'
import routes from '../../fixtures/manual-routes.json'
import { bodyOf, hostnameError, inPrefix, needsNodeQuestion, parseIPv4, validate, valuesOf } from './manual'

const stored = (routes as ManualRouteView[])[0] as ManualRouteView
const cidrs = ['10.0.5.0/24']

describe('the hostname, as hostname.Normalize takes it', () => {
  test.each([
    ['status.example.com', undefined],
    ['Status.Example.COM.', undefined],
    ['*.example.com', undefined],
    ['', 'empty'],
    ['example', 'needs at least two labels'],
    ['*.com', 'a wildcard needs at least two labels after the *'],
    ['a..example.com', 'empty label'],
    ['a_b.example.com', `label "a_b" contains '_'`],
    ['-a.example.com', 'label "-a" starts or ends with a hyphen'],
    ['10.0.5.20', 'last label "20" is all digits'],
    [`${'a'.repeat(64)}.example.com`, `label "${'a'.repeat(64)}" is longer than 63 characters`],
    [`${'a.'.repeat(127)}com`, 'longer than 253 characters'],
    ['bücher.example.com', `label "bücher" contains 'ü'`],
    ['a\u202eb.example.com', String.raw`label "a\u202eb" contains '\u202e'`],
  ])('%s', (name, want) => {
    expect(hostnameError(name)).toBe(want)
  })
})

test('addresses as netip.ParseAddr reads them, and prefixes', () => {
  expect(parseIPv4('10.0.5.20')).toBe(10 * 2 ** 24 + 5 * 256 + 20)
  for (const bad of ['10.0.5', '10.0.5.256', '10.0.05.20', '::1', 'a.b.c.d', '']) expect(parseIPv4(bad), bad).toBeUndefined()
  const addr = parseIPv4('10.0.5.20') as number
  expect(inPrefix(addr, '10.0.5.0/24')).toBe(true)
  expect(inPrefix(addr, '10.0.4.0/24')).toBe(false)
  expect(inPrefix(addr, '0.0.0.0/0')).toBe(true)
  expect(inPrefix(addr, '10.0.5.20/32')).toBe(true)
  expect(inPrefix(addr, 'fd00::/8')).toBe(false)
})

describe('the form', () => {
  test('a stored route reads back as it was', () => {
    expect(valuesOf(stored)).toMatchObject({ id: 'status', hostname: 'status.example.com', kind: 'address', addr: '10.0.5.20', port: '9000', scheme: 'http' })
    expect(validate(valuesOf(stored), { isNew: false, manualCIDRs: cidrs })).toEqual({})
  })

  test('each field says what is wrong with it', () => {
    const v = { ...valuesOf(), id: 'Bad Id', hostname: 'example', addr: '10.0.6.1', port: '70000', hostHeader: 'a b', scheme: 'https' as const, sni: '*.example.com' }
    expect(validate(v, { isNew: true, manualCIDRs: cidrs })).toEqual({
      id: 'want 1 to 32 of a-z, 0-9 and -, or nothing for an id of its own',
      hostname: 'needs at least two labels',
      port: 'want a port from 1 to 65535',
      addr: 'not inside the manualCIDRs of the settings (10.0.5.0/24)',
      hostHeader: '"a b" is not a host name with an optional port',
      sni: '"*.example.com" is not a host name',
    })
  })

  test('without manualCIDRs no address is allowed; while they are not known the daemon checks', () => {
    const v = valuesOf(stored)
    expect(validate(v, { isNew: false, manualCIDRs: [] }).addr).toBe('not inside the manualCIDRs of the settings (none)')
    expect(validate(v, { isNew: false }).addr).toBeUndefined()
  })

  test('a guest target: a guest chosen, and via as the planner takes it', () => {
    const v = { ...valuesOf(), hostname: 'app.example.com', kind: 'guest' as const, port: '3000' }
    expect(validate(v, { isNew: true, manualCIDRs: cidrs })).toEqual({ guest: 'choose a guest' })
    for (const via of ['net0', 'NET31', '10.0.0.11']) expect(validate({ ...v, guest: 'qemu/101', via }, { isNew: true }), via).toEqual({})
    for (const via of ['net32', 'net01', '127.0.0.1', '169.254.1.1', '224.0.0.1', 'eth0']) {
      expect(validate({ ...v, guest: 'qemu/101', via }, { isNew: true }).via, via).toBe(`"${via}" is neither a NIC from net0 to net31 nor an IPv4 address a guest can have`)
    }
    // quoted as the daemon quotes it
    expect(validate({ ...v, guest: 'qemu/101', via: 'net"1' }, { isNew: true }).via).toBe(
      String.raw`"net\"1" is neither a NIC from net0 to net31 nor an IPv4 address a guest can have`,
    )
  })

  test('the body: what applies to the target only, the id of a new route when given, and the revision', () => {
    const v = { ...valuesOf(stored), hostname: ' Status.Example.com. ', via: 'net1', noTLSVerify: true, sni: 'x.example.com', allowNode: true }
    expect(bodyOf(v, false, 2)).toEqual({
      rev: 2,
      hostname: 'status.example.com',
      target: { kind: 'address', scheme: 'http', addr: '10.0.5.20', port: 9000 },
      options: { allowNode: true },
    })
    const guest = { ...valuesOf(), id: 'app', hostname: 'app.example.com', kind: 'guest' as const, guest: 'qemu/101', port: '443', scheme: 'https' as const, sni: 'app.internal.example.com', via: 'net1', allowNode: true }
    expect(bodyOf(guest, true)).toEqual({
      id: 'app',
      hostname: 'app.example.com',
      target: { kind: 'guest', scheme: 'https', guest: 'qemu/101', port: 443 },
      options: { sni: 'app.internal.example.com', via: 'net1' },
    })
    expect(bodyOf({ ...guest, id: '' }, true)).not.toHaveProperty('id')
  })

  test('allowNode asks again when it publishes a service of a node that was not published so', () => {
    const v = { ...valuesOf(stored), allowNode: true }
    expect(needsNodeQuestion(v)).toBe(true)
    expect(needsNodeQuestion(v, stored)).toBe(true)
    const already = { ...stored, options: { allowNode: true } }
    expect(needsNodeQuestion(v, already)).toBe(false)
    expect(needsNodeQuestion({ ...v, port: '9001' }, already)).toBe(true)
    expect(needsNodeQuestion({ ...v, allowNode: false }, stored)).toBe(false)
  })
})
