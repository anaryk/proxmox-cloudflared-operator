import { describe, expect, test } from 'vitest'

import { blockErrors, type BlockValues, buildBlock, emptyBlock, hostOf } from './blockBuilder'

const base: BlockValues = { ...emptyBlock, hostname: 'app.example.com', port: '3000' }

describe('the block', () => {
  test('a plain route is a fenced cf-tunnel block of one line', () => {
    expect(buildBlock(base)).toBe('```cf-tunnel\napp.example.com -> :3000\n```')
  })

  test('https puts the scheme before the colon, and its options follow the target in the order of the documentation', () => {
    expect(buildBlock({ ...base, scheme: 'https', port: '8443', noTLSVerify: true, hostHeader: 'admin.internal:8443', sni: 'Admin.Internal.Example.com', via: 'net1' })).toBe(
      '```cf-tunnel\napp.example.com -> https://:8443 no-tls-verify host-header=admin.internal:8443 sni=admin.internal.example.com via=net1\n```',
    )
  })

  test('the options of https are left out of an http route, whatever was typed', () => {
    expect(buildBlock({ ...base, noTLSVerify: true, sni: 'x.example.com', hostHeader: 'h.example.com' })).toBe('```cf-tunnel\napp.example.com -> :3000 host-header=h.example.com\n```')
  })

  test('the name is written as the parser reads it: lower case, no trailing dot, trimmed', () => {
    expect(buildBlock({ ...base, hostname: ' App.Example.COM. ', port: ' 3000 ' })).toBe('```cf-tunnel\napp.example.com -> :3000\n```')
  })

  test('via takes a NIC or an address of the guest', () => {
    expect(buildBlock({ ...base, via: 'NET0' })).toContain(' via=net0\n')
    expect(buildBlock({ ...base, via: '10.0.0.50' })).toContain(' via=10.0.0.50\n')
  })

  test('a wildcard is a route like another; the daemon says whether the settings allow it', () => {
    expect(buildBlock({ ...base, hostname: '*.shop.example.com', port: '80' })).toBe('```cf-tunnel\n*.shop.example.com -> :80\n```')
  })
})

describe('what the parser would drop is no block', () => {
  test.each<[string, Partial<BlockValues>, keyof BlockValues]>([
    ['a name of one label', { hostname: 'wiki' }, 'hostname'],
    ['an empty name', { hostname: '' }, 'hostname'],
    ['an underscore', { hostname: 'my_app.example.com' }, 'hostname'],
    ['a port above 65535', { port: '65536' }, 'port'],
    ['port 0', { port: '0' }, 'port'],
    ['a leading zero', { port: '080' }, 'port'],
    ['no port', { port: '' }, 'port'],
    ['a word for a port', { port: 'http' }, 'port'],
    ['a header with a space', { hostHeader: 'a b' }, 'hostHeader'],
    ['a sni that is a wildcard', { scheme: 'https', sni: '*.example.com' }, 'sni'],
    ['a via that is no NIC and no address', { via: 'eth0' }, 'via'],
    ['a via of a loopback address', { via: '127.0.0.1' }, 'via'],
    ['a NIC above net31', { via: 'net32' }, 'via'],
  ])('%s', (_, patch, field) => {
    const v = { ...base, ...patch }
    expect(blockErrors(v)[field]).toBeTruthy()
    expect(buildBlock(v)).toBeUndefined()
  })

  test('a sni on an http route is not looked at: it is not written', () => {
    expect(blockErrors({ ...base, sni: '*' }).sni).toBeUndefined()
  })
})

test('a name goes in front of a zone', () => {
  expect(hostOf('app', 'example.com')).toBe('app.example.com')
  expect(hostOf(' app. ', 'example.com')).toBe('app.example.com')
  expect(hostOf('', 'example.com')).toBe('')
})
