import { describe, expect, test } from 'vitest'

import { match, navigate, sectionOf } from './router'

describe('every path of spec-ui 3.2', () => {
  test.each([
    ['/', '', { name: 'overview' }],
    ['/', '?focus=guest:qemu/101', { name: 'overview' }],
    ['/routes', '', { name: 'routes' }],
    ['/routes/www.example.com', '', { name: 'route', hostname: 'www.example.com' }],
    ['/routes/www.example.com', '?owner=qemu/101', { name: 'route', hostname: 'www.example.com', owner: 'qemu/101' }],
    ['/routes/xn--bcher-kva.example', '', { name: 'route', hostname: 'xn--bcher-kva.example' }],
    ['/routes/plan', '', { name: 'plan' }],
    ['/routes/manual/new', '', { name: 'manual-new' }],
    ['/routes/manual/status', '', { name: 'manual', id: 'status' }],
    ['/guests', '', { name: 'guests' }],
    ['/guests/qemu/101', '', { name: 'guest', kind: 'qemu', vmid: 101 }],
    ['/guests/lxc/200', '', { name: 'guest', kind: 'lxc', vmid: 200 }],
    ['/guests/claims', '', { name: 'claims' }],
    ['/networks', '', { name: 'networks' }],
    ['/edge/credentials', '', { name: 'credentials' }],
    ['/edge/credentials/cred1', '', { name: 'credential', id: 'cred1' }],
    ['/edge/zones', '', { name: 'zones' }],
    ['/edge/zones/example.com', '', { name: 'zone', zone: 'example.com' }],
    ['/edge/tunnels', '', { name: 'tunnels' }],
    ['/edge/tunnels/023e105f4ecef8ad9ca31a8372d0c353', '', { name: 'tunnel', account: '023e105f4ecef8ad9ca31a8372d0c353' }],
    ['/events', '?route=www.example.com&level=error', { name: 'events' }],
    ['/doctor', '', { name: 'doctor' }],
    ['/settings', '', { name: 'settings' }],
    ['/setup', '', { name: 'setup' }],
    ['/signin', '', { name: 'signin' }],
  ])('%s%s', (path, search, view) => {
    expect(match(path, search)).toEqual(view)
  })

  test('a slash at the end is the same view', () => {
    expect(match('/routes/')).toEqual({ name: 'routes' })
  })

  test.each([
    '/nothing',
    '/routes/plan/x',
    '/routes/manual',
    '/routes/manual/a/b',
    '/routes/a%2Fb',
    '/routes/%E0%A4%A',
    '/guests/vm/101',
    '/guests/qemu/abc',
    '/guests/qemu/101/annotation',
    '/edge',
    '/edge/accounts',
    '/edge/zones/a/b',
    '/events/x',
    '/doctor/x',
    '/assets/index.js',
  ])('%s is no view', (path) => {
    expect(match(path)).toEqual({ name: 'not-found' })
  })
})

test('the section of the navigation each view belongs to', () => {
  expect(sectionOf(match('/routes/plan'))).toBe('routes')
  expect(sectionOf(match('/guests/claims'))).toBe('guests')
  expect(sectionOf(match('/edge/zones/example.com'))).toBe('zones')
  expect(sectionOf(match('/setup'))).toBe('settings')
  expect(sectionOf(match('/signin'))).toBeUndefined()
})

test('navigate changes the address without loading the page', () => {
  navigate('/routes?x=1')
  expect(window.location.pathname + window.location.search).toBe('/routes?x=1')
  navigate('/events', true)
  expect(window.location.pathname).toBe('/events')
})
