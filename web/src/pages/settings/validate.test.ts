import { describe, expect, test } from 'vitest'

import settingsFixture from '../../fixtures/settings.json'
import type { Settings } from '../../api/types.gen'
import { containsAddr, normalizeHostname, normalizePattern, parseIPv4, parsePrefix, validateSettings } from './validate'

const view = settingsFixture as unknown as { settings: Settings; limits: Record<string, { min?: unknown; max?: unknown }> }
const limits = view.limits

// The settings the store tests start from: every field valid.
const good: Settings = {
  ...view.settings,
  allowHosts: ['*.example.com'],
  denyHosts: ['secret.example.com'],
  trustedCIDRs: ['10.0.20.0/24'],
  manualCIDRs: ['10.0.5.0/24'],
  zonePins: { 'example.com': 'a1b2c3d4' },
}

const first = (s: Settings, l = limits) => validateSettings(s, l)[0]

test('valid settings have no issue, and the fixture is valid', () => {
  expect(validateSettings(good, limits)).toEqual([])
  expect(validateSettings(view.settings, limits)).toEqual([])
})

// The cases of internal/store/settings_test.go, each with the field the
// store names.
describe('what the store refuses, the page refuses at the same field', () => {
  const cases: [string, string, Partial<Settings>][] = [
    ['empty gate tag', 'gateTag', { gateTag: '' }],
    ['upper case gate tag', 'gateTag', { gateTag: 'CF-Tunnel' }],
    ['gate tag with a space', 'gateTag', { gateTag: 'cf tunnel' }],
    ['gate tag with a semicolon', 'gateTag', { gateTag: 'a;b' }],
    ['gate tag starting with a hyphen', 'gateTag', { gateTag: '-a' }],
    ['gate tag starting with a dot', 'gateTag', { gateTag: '.a' }],
    ['gate tag starting with a plus', 'gateTag', { gateTag: '+a' }],
    ['gate tag over 64 characters', 'gateTag', { gateTag: 'a'.repeat(65) }],
    ['bad allow pattern', 'allowHosts[1]', { allowHosts: ['*.example.com', 'not a host'] }],
    ['single label allow pattern', 'allowHosts[0]', { allowHosts: ['localhost'] }],
    ['bad deny pattern', 'denyHosts[0]', { denyHosts: ['a..b'] }],
    ['empty deny pattern', 'denyHosts[0]', { denyHosts: [''] }],
    ['zero poll interval', 'pollInterval', { pollInterval: '0' }],
    ['negative poll interval', 'pollInterval', { pollInterval: '-1s' }],
    ['poll interval that is no duration', 'pollInterval', { pollInterval: 'ten' }],
    ['zero grace', 'grace', { grace: '0s' }],
    ['negative grace', 'grace', { grace: '-1m' }],
    ['empty admission', 'admission', { admission: '' }],
    ['unknown admission', 'admission', { admission: 'open' }],
    ['upper case admission', 'admission', { admission: 'Tag' }],
    ['bad zone pin key', 'zonePins["not a zone"]', { zonePins: { 'not a zone': 'c' } }],
    ['empty zone pin key', 'zonePins[""]', { zonePins: { '': 'c' } }],
    ['empty zone pin value', 'zonePins["example.com"]', { zonePins: { 'example.com': '' } }],
    ['zone pin keys that collide', 'zonePins["example.com"]', { zonePins: { 'Example.com': 'a', 'example.com': 'b' } }],
    ['ipv6 trusted prefix', 'trustedCIDRs[0]', { trustedCIDRs: ['fd00::/8'] }],
    ['mapped trusted prefix', 'trustedCIDRs[0]', { trustedCIDRs: ['::ffff:10.0.0.0/104'] }],
    ['empty trusted prefix', 'trustedCIDRs[0]', { trustedCIDRs: [''] }],
    ['trusted prefix with no length', 'trustedCIDRs[0]', { trustedCIDRs: ['10.0.0.0'] }],
    ['trusted prefix of 33 bits', 'trustedCIDRs[0]', { trustedCIDRs: ['10.0.0.0/33'] }],
    ['ipv6 manual prefix', 'manualCIDRs[0]', { manualCIDRs: ['fd00::/8'] }],
    ['mapped manual prefix', 'manualCIDRs[0]', { manualCIDRs: ['::ffff:10.0.0.0/104'] }],
    ['empty manual prefix', 'manualCIDRs[0]', { manualCIDRs: [''] }],
    ['empty identity minimum', 'identityMinimum', { identityMinimum: '' }],
    ['unknown identity minimum', 'identityMinimum', { identityMinimum: 'strict' }],
    ['upper case identity minimum', 'identityMinimum', { identityMinimum: 'Port' }],
    ['manual as identity minimum', 'identityMinimum', { identityMinimum: 'manual' }],
    ['a cap that is no whole number', 'maxHostnamesPerGuest', { maxHostnamesPerGuest: 1.5 }],
    ['a cap that is no number', 'maxHostnamesPerGuest', { maxHostnamesPerGuest: Number.NaN }],
    ['a budget that is no whole number', 'cloudflareBudget', { cloudflareBudget: 100.5 }],
  ]
  test.each(cases)('%s', (_name, field, edit) => {
    const got = first({ ...good, ...edit })
    expect(got?.field).toBe(field)
    expect(got?.message).not.toBe('')
  })

  test('the first issue is the one the store names first', () => {
    const got = validateSettings({ ...good, admission: 'open', gateTag: '', pollInterval: '1s' }, limits)
    expect(got.map((i) => i.field)).toEqual(['gateTag', 'pollInterval', 'admission'])
  })

  test('every bad entry of a list is named, not only the first', () => {
    const got = validateSettings({ ...good, denyHosts: ['a..b', 'ok.example.com', 'c d'] }, limits)
    expect(got.map((i) => i.field)).toEqual(['denyHosts[0]', 'denyHosts[2]'])
    expect(got[0]?.message).toContain('"a..b"')
  })
})

describe('the ranges are the limits of the answer', () => {
  const field = (name: keyof Settings, value: unknown, edit?: Record<string, { min?: unknown; max?: unknown }>) =>
    validateSettings({ ...good, [name]: value } as Settings, { ...limits, ...edit })

  test.each([
    ['pollInterval', '5s', true],
    ['pollInterval', '4.999s', false],
    ['pollInterval', '1m', true],
    ['grace', '30s', true],
    ['grace', '29s', false],
    ['grace', '60ms', false],
    ['reverifyInterval', '10s', true],
    ['reverifyInterval', '9s', false],
    ['reverifyInterval', '0s', false],
    ['reverifyInterval', '5m0s', true],
    ['reverifyInterval', '5m', true],
    ['reverifyInterval', '300s', true],
    ['reverifyInterval', '5m1s', false],
    ['maxHostnamesPerGuest', 1, true],
    ['maxHostnamesPerGuest', 0, false],
    ['maxHostnamesPerGuest', -1, false],
    ['maxHostnamesPerGuest', 5000, true],
    ['cloudflareBudget', 100, true],
    ['cloudflareBudget', 99, false],
    ['cloudflareBudget', 0, false],
    ['cloudflareBudget', 1150, true],
    ['cloudflareBudget', 1151, false],
  ] as const)('%s %s is %s', (name, value, valid) => {
    const got = field(name, value)
    expect(got.map((i) => i.field)).toEqual(valid ? [] : [name])
  })

  test('the messages say the range in the words of the answer', () => {
    expect(field('pollInterval', '1s')[0]?.message).toBe('pollInterval 1s: at least 5s')
    expect(field('grace', '29s')[0]?.message).toBe('grace 29s: at least 30s')
    expect(field('reverifyInterval', '9s')[0]?.message).toBe('reverifyInterval 9s: at least 10s')
    expect(field('reverifyInterval', '5m1s')[0]?.message).toBe('reverifyInterval 5m1s: at most 5m0s')
    expect(field('maxHostnamesPerGuest', 0)[0]?.message).toBe('maxHostnamesPerGuest 0: at least 1')
    expect(field('cloudflareBudget', 99)[0]?.message).toBe('cloudflareBudget 99: from 100 to 1150')
  })

  test('a range the daemon changes is the range the page checks', () => {
    const wider = {
      pollInterval: { min: '1s' },
      cloudflareBudget: { min: 10, max: 2000 },
      reverifyInterval: { min: '1s', max: '1h0m0s' },
      maxHostnamesPerGuest: { min: 4 },
    }
    expect(field('pollInterval', '1s', wider)).toEqual([])
    expect(field('cloudflareBudget', 2000, wider)).toEqual([])
    expect(field('cloudflareBudget', 2001, wider)[0]?.message).toBe('cloudflareBudget 2001: from 10 to 2000')
    expect(field('reverifyInterval', '1h', wider)).toEqual([])
    expect(field('maxHostnamesPerGuest', 3, wider)[0]?.message).toBe('maxHostnamesPerGuest 3: at least 4')
    const narrower = { cloudflareBudget: { min: 500, max: 600 } }
    expect(field('cloudflareBudget', 1000, narrower)[0]?.message).toBe('cloudflareBudget 1000: from 500 to 600')
  })

  test('a limit the answer does not give is not checked by the page', () => {
    expect(validateSettings({ ...good, cloudflareBudget: 5000, pollInterval: '1ms' }, {})).toEqual([])
  })
})

describe('observe-only has one way out', () => {
  test('leaving it is refused with the daemon words, staying or entering is not', () => {
    const was = { ...good, observeOnly: true }
    const got = validateSettings({ ...good, observeOnly: false }, limits, was)
    expect(got).toEqual([{ field: 'observeOnly', message: expect.stringContaining('use apply') }])
    expect(validateSettings({ ...good, observeOnly: true }, limits, was)).toEqual([])
    expect(validateSettings({ ...good, observeOnly: true }, limits, { ...good, observeOnly: false })).toEqual([])
    expect(validateSettings({ ...good, observeOnly: false }, limits, { ...good, observeOnly: false })).toEqual([])
  })
})

describe('hostnames and patterns, as internal/hostname', () => {
  test.each([
    ['Example.COM', 'example.com'],
    ['Example.com.', 'example.com'],
    ['a.b.example.com', 'a.b.example.com'],
    ['*.Example.com', '*.example.com'],
    ['xn--bcher-kva.example', 'xn--bcher-kva.example'],
    ['a-b.example.com', 'a-b.example.com'],
  ])('%s is %s', (input, want) => {
    expect(normalizeHostname(input)).toEqual({ value: want })
  })

  test.each([
    ['', 'empty'],
    ['localhost', 'needs at least two labels'],
    ['*.com', 'a wildcard needs at least two labels after the *'],
    ['a..b', 'empty label'],
    ['-a.example.com', 'starts or ends with a hyphen'],
    ['a-.example.com', 'starts or ends with a hyphen'],
    ['a b.example.com', 'contains " "'],
    ['bücher.example', 'contains "ü"'],
    ['a.*.example.com', 'contains "*"'],
    ['10.0.0.5', 'last label "5" is all digits'],
    [`${'a'.repeat(64)}.example.com`, 'is longer than 63 characters'],
    [`${'a.'.repeat(130)}com`, 'longer than 253 characters'],
  ])('%j is refused: %s', (input, why) => {
    const got = normalizeHostname(input)
    expect(got.value).toBeUndefined()
    expect(got.error).toContain(why)
  })

  test.each([
    ['*', '*'],
    ['*.com', '*.com'],
    ['*.Example.COM.', '*.example.com'],
    ['Shop.cz', 'shop.cz'],
    ['*.a.b.example.com', '*.a.b.example.com'],
  ])('the pattern %s is %s', (input, want) => {
    expect(normalizePattern(input)).toEqual({ value: want })
  })

  test.each([
    ['*.', 'a bare * takes no trailing dot'],
    ['', 'empty'],
    ['localhost', 'needs at least two labels'],
    ['*.*.com', 'contains "*"'],
    ['**', 'needs at least two labels'],
    ['a*.example.com', 'contains "*"'],
  ])('the pattern %j is refused: %s', (input, why) => {
    expect(normalizePattern(input).error).toContain(why)
  })
})

describe('addresses and prefixes', () => {
  test.each(['10.0.5.20', '0.0.0.0', '255.255.255.255', '192.168.1.1'])('%s is an IPv4 address', (a) => {
    expect(parseIPv4(a)).toBe(a)
  })

  test.each(['', '10.0.5', '10.0.5.256', '10.0.5.20.1', '010.0.5.20', '10.0.5.-1', ' 10.0.5.20', '::1', '10.0.5.20/24', '1e1.0.0.1', '10.0.5.0x1'])(
    '%j is none',
    (a) => {
      expect(parseIPv4(a)).toBeUndefined()
    },
  )

  test.each(['10.0.5.0/24', '0.0.0.0/0', '10.0.5.7/32', '10.0.5.7/24', '192.168.0.0/16'])('%s is a prefix', (p) => {
    expect(parsePrefix(p)).toBeDefined()
  })

  test.each(['', '10.0.5.0', '10.0.5.0/', '10.0.5.0/33', '10.0.5.0/-1', '10.0.5.0/024', '10.0.5.0/+8', 'fd00::/8', '::ffff:10.0.0.0/104', '10.0.5/24', '10.0.5.0/24/1'])(
    '%j is no IPv4 prefix',
    (p) => {
      expect(parsePrefix(p)).toBeUndefined()
    },
  )

  test('a prefix contains the addresses of its network, whatever bits the text has below the length', () => {
    const net = parsePrefix('10.0.5.7/24')
    expect(net && containsAddr(net, '10.0.5.200')).toBe(true)
    expect(net && containsAddr(net, '10.0.6.1')).toBe(false)
    const all = parsePrefix('0.0.0.0/0')
    expect(all && containsAddr(all, '203.0.113.9')).toBe(true)
    const one = parsePrefix('10.0.5.7/32')
    expect(one && containsAddr(one, '10.0.5.7')).toBe(true)
    expect(one && containsAddr(one, '10.0.5.8')).toBe(false)
    const high = parsePrefix('255.255.255.0/24')
    expect(high && containsAddr(high, '255.255.255.9')).toBe(true)
  })
})
