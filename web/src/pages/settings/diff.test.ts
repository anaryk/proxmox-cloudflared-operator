import { describe, expect, test } from 'vitest'

import type { ManualRouteView, Settings } from '../../api/types.gen'
import manualFixture from '../../fixtures/manual-routes.json'
import settingsFixture from '../../fixtures/settings.json'
import { diffSettings, optInWords, planRoutes, routeChanges, routeText, targetText } from './diff'

const stored = settingsFixture.settings as unknown as Settings
const [status] = manualFixture as unknown as ManualRouteView[]
if (!status) throw new Error('the fixture has no manual route')

describe('the settings', () => {
  test('the same settings differ in nothing', () => {
    expect(diffSettings(stored, stored)).toEqual([])
    expect(diffSettings(stored, { ...stored, allowHosts: [], trustStatic: false, zonePins: {} })).toEqual([])
  })

  test('a value that changed is shown with what it was', () => {
    expect(diffSettings(stored, { ...stored, pollInterval: '30s', cloudflareBudget: 500 })).toEqual([
      { field: 'pollInterval', before: '10s', after: '30s' },
      { field: 'cloudflareBudget', before: '1000', after: '500' },
    ])
  })

  test('a flag is true or false, and a flag the daemon leaves out is false', () => {
    expect(diffSettings(stored, { ...stored, trustStatic: true, observeOnly: true })).toEqual([
      { field: 'trustStatic', before: 'false', after: 'true' },
      { field: 'observeOnly', before: 'false', after: 'true' },
    ])
  })

  test('the changes come in the order of the settings, whatever order the fields were written in', () => {
    const after: Settings = {
      cloudflareBudget: 500,
      identityMinimum: 'observed',
      observeOnly: true,
      allowHosts: ['a.example'],
      gateTag: 'pco',
      reverifyInterval: '1m0s',
      maxHostnamesPerGuest: 32,
      admission: 'tag',
      grace: '1m0s',
      pollInterval: '10s',
    }
    expect(diffSettings(stored, after).map((c) => c.field)).toEqual(['gateTag', 'allowHosts', 'observeOnly', 'identityMinimum', 'cloudflareBudget'])
  })

  test('a list shows what was added and what was removed', () => {
    const before = { ...stored, allowHosts: ['*.example.com', 'shop.cz'] }
    const after = { ...stored, allowHosts: ['shop.cz', 'example.com'], denyHosts: ['x.example.com'] }
    expect(diffSettings(before, after)).toEqual([
      { field: 'allowHosts', before: '*.example.com, shop.cz', after: 'shop.cz, example.com', added: ['example.com'], removed: ['*.example.com'] },
      { field: 'denyHosts', before: 'none', after: 'x.example.com', added: ['x.example.com'], removed: [] },
    ])
  })

  test('the order of a list is no change', () => {
    const before = { ...stored, trustedCIDRs: ['10.0.1.0/24', '10.0.2.0/24'] }
    expect(diffSettings(before, { ...before, trustedCIDRs: ['10.0.2.0/24', '10.0.1.0/24'] })).toEqual([])
  })

  test('a zone pin is a zone and its credential, and a pin that moved is removed and added', () => {
    const before = { ...stored, zonePins: { 'example.com': 'a1', 'shop.cz': 'b2' } }
    const after = { ...stored, zonePins: { 'example.com': 'c3', 'shop.cz': 'b2', 'new.example': 'd4' } }
    expect(diffSettings(before, after)).toEqual([
      {
        field: 'zonePins',
        before: 'example.com → a1, shop.cz → b2',
        after: 'example.com → c3, new.example → d4, shop.cz → b2',
        added: ['example.com → c3', 'new.example → d4'],
        removed: ['example.com → a1'],
      },
    ])
  })
})

const route = (over: Partial<ManualRouteView> = {}): ManualRouteView => ({ ...status, ...over })

describe('manual routes', () => {
  test('a target is written as its URL, a guest by its ref', () => {
    expect(targetText(status.target)).toBe('http://10.0.5.20:9000')
    expect(targetText({ kind: 'guest', guest: 'qemu/101', scheme: 'https', port: 8443 })).toBe('https://qemu/101:8443')
    expect(routeText(status)).toBe('status.example.com → http://10.0.5.20:9000')
  })

  const web = route({ id: 'web', hostname: 'web.example.com', target: { kind: 'guest', guest: 'qemu/101', scheme: 'http', port: 80 } })
  const old = route({ id: 'old', hostname: 'old.example.com' })

  test('merge adds what is new, updates what differs, and deletes nothing', () => {
    const changed = route({ target: { ...status.target, port: 9001 } })
    const plan = planRoutes([status, old], [changed, web], 'merge')
    expect(plan.add).toEqual([web])
    expect(plan.update.map((u) => [u.before, u.after])).toEqual([[status, changed]])
    expect(plan.update[0]?.changes).toEqual(['target http://10.0.5.20:9000 → http://10.0.5.20:9001'])
    expect(plan.same).toEqual([])
    expect(plan.remove).toEqual([])
  })

  test('replace deletes the routes the file lacks, by id', () => {
    const plan = planRoutes([old, status], [status, web], 'replace')
    expect(plan.add).toEqual([web])
    expect(plan.same).toEqual([status])
    expect(plan.update).toEqual([])
    expect(plan.remove).toEqual([old])
  })

  test('the revision of the file is no difference, nor an option left out and one that is false', () => {
    const same = route({ rev: 99, options: { noTLSVerify: false, hostHeader: '', allowNode: false } })
    const plan = planRoutes([status], [same], 'merge')
    expect(plan.same).toEqual([status])
    expect(plan.update).toEqual([])
  })

  test('what differs is named: the hostname, the target and each option', () => {
    const before = route({ options: { hostHeader: 'a.example.com' } })
    const after = route({
      hostname: 'status.example.org',
      target: { kind: 'address', scheme: 'https', addr: '10.0.5.21', port: 9443 },
      options: { noTLSVerify: true, sni: 'x.example.com', allowNode: true },
    })
    expect(routeChanges(before, after)).toEqual([
      'hostname status.example.com → status.example.org',
      'target http://10.0.5.20:9000 → https://10.0.5.21:9443',
      'noTLSVerify false → true',
      'hostHeader a.example.com → none',
      'sni none → x.example.com',
      'allowNode false → true',
    ])
  })
})

describe('what an opt-in grants', () => {
  test('the apex of a zone', () => {
    expect(optInWords('example.com', 'qemu/101')).toBe(
      'Whoever may edit the Notes of qemu/101, or of any other tagged guest, may then publish example.com, the apex of its zone, with a valid certificate.',
    )
  })

  test('a wildcard answers the names that have no record of their own', () => {
    expect(optInWords('*.example.com', 'lxc/200')).toBe(
      'Whoever may edit the Notes of lxc/200, or of any other tagged guest, may then publish *.example.com: the wildcard that answers every name below example.com that has no record of its own, with a valid certificate.',
    )
  })
})
