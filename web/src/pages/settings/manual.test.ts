import { describe, expect, test } from 'vitest'

import type { ManualRouteView, ManualTarget, Settings } from '../../api/types.gen'
import generated from '../../api/types.gen.ts?raw'
import manualFixture from '../../fixtures/manual-routes.json'
import settingsFixture from '../../fixtures/settings.json'
import { checkImport, normalizeRoute, parseImportFile, validateRoute } from './manual'
import { settingFields } from './validate'

const limits = settingsFixture.limits as Record<string, { min?: unknown; max?: unknown }>
const stored = { ...(settingsFixture.settings as unknown as Settings), manualCIDRs: ['10.0.5.0/24'] }
const routes = manualFixture as unknown as ManualRouteView[]

const omit = (o: object, ...keys: string[]) => Object.fromEntries(Object.entries(o).filter(([k]) => !keys.includes(k)))

const file = (over: Record<string, unknown> = {}) => JSON.stringify({ rev: 7, settings: stored, manualRoutes: routes, ...over })
const check = (text: string, current: Settings = stored) => checkImport(text, { current, limits })

describe('the keys of a settings file are the fields of the generated type', () => {
  const body = /export interface Settings \{([^}]*)\}/.exec(generated)?.[1] ?? ''
  const fields = [...body.matchAll(/^\s+(\w+)(\??):/gm)].map((m) => ({ name: m[1], optional: m[2] === '?' }))

  test('in the order of the settings, and the ones the daemon may leave out', () => {
    expect(fields.length).toBeGreaterThan(10)
    expect(settingFields.map((f) => ({ name: f.name, optional: f.optional }))).toEqual(fields)
  })
})

describe('reading a file', () => {
  test('the file this page exports is read', () => {
    const got = parseImportFile(file())
    expect(got.issues).toEqual([])
    expect(got.file?.rev).toBe(7)
    expect(got.file?.settings).toEqual(stored)
    expect(got.file?.routes).toEqual(routes)
  })

  test('what pco settings show --json prints is read, with no manual routes in it', () => {
    const shown = JSON.stringify({ ...settingsFixture, settings: stored })
    const got = parseImportFile(shown)
    expect(got.issues).toEqual([])
    expect(got.file?.routes).toBeUndefined()
  })

  test('a file that is no JSON says so and does not repeat itself', () => {
    const got = parseImportFile('{"settings": SECRET')
    expect(got.file).toBeUndefined()
    expect(got.issues).toEqual([{ where: 'file', message: 'this is not JSON' }])
  })

  test.each([
    ['[]', 'the file must be a JSON object'],
    ['null', 'the file must be a JSON object'],
    ['"settings"', 'the file must be a JSON object'],
  ])('%s is refused', (text, message) => {
    expect(parseImportFile(text).issues).toEqual([{ where: 'file', message }])
  })

  test('a key the file has no place for is named', () => {
    expect(parseImportFile(file({ extra: 1 })).issues).toEqual([{ where: 'extra', message: 'unknown key' }])
  })

  test('the settings are needed', () => {
    expect(parseImportFile('{"rev": 3}').issues).toEqual([{ where: 'settings', message: 'missing' }])
    expect(parseImportFile('{"settings": []}').issues).toEqual([{ where: 'settings', message: 'want an object' }])
  })

  test('a setting the daemon does not know is refused as the daemon refuses it', () => {
    const got = parseImportFile(file({ settings: { ...stored, denyhost: ['a.example.com'] } }))
    expect(got.issues).toEqual([{ where: 'settings.denyhost', message: 'unknown setting' }])
  })

  test('a setting the daemon needs is named when it is missing', () => {
    expect(parseImportFile(file({ settings: omit(stored, 'cloudflareBudget', 'observeOnly') })).issues).toEqual([
      { where: 'settings.observeOnly', message: 'missing' },
      { where: 'settings.cloudflareBudget', message: 'missing' },
    ])
  })

  test.each([
    ['pollInterval', 10, 'want text'],
    ['gateTag', null, 'want text'],
    ['cloudflareBudget', 1.5, 'want a whole number'],
    ['cloudflareBudget', '1000', 'want a whole number'],
    ['observeOnly', 'false', 'want true or false'],
    ['allowHosts', 'a.example.com', 'want a list of text'],
    ['allowHosts', ['a.example.com', 3], 'want a list of text'],
    ['zonePins', ['a'], 'want an object of text'],
    ['zonePins', { 'a.example': 1 }, 'want an object of text'],
  ])('%s of %j: %s', (name, value, message) => {
    const got = parseImportFile(file({ settings: { ...stored, [name]: value } }))
    expect(got.issues).toEqual([{ where: `settings.${name}`, message }])
  })

  test('a list that is null is none', () => {
    const got = parseImportFile(file({ settings: { ...stored, allowHosts: null, zonePins: null } }))
    expect(got.issues).toEqual([])
    expect(got.file?.settings.allowHosts).toBeUndefined()
  })

  test('the routes are a list of objects with the keys of a route', () => {
    expect(parseImportFile(file({ manualRoutes: {} })).issues).toEqual([{ where: 'manualRoutes', message: 'want a list' }])
    expect(parseImportFile(file({ manualRoutes: [3] })).issues).toEqual([{ where: 'manualRoutes[0]', message: 'want an object' }])
    const [route] = routes
    const issues = (r: unknown) => parseImportFile(file({ manualRoutes: [r] })).issues
    expect(issues({ ...route, extra: 1 })).toEqual([{ where: 'manualRoutes[0].extra', message: 'unknown key' }])
    expect(issues({ ...route, target: { ...route?.target, color: 'red' } })).toEqual([{ where: 'manualRoutes[0].target.color', message: 'unknown key' }])
    expect(issues({ ...route, options: { fast: true } })).toEqual([{ where: 'manualRoutes[0].options.fast', message: 'unknown key' }])
    expect(issues({ ...route, target: { ...route?.target, port: '9000' } })).toEqual([{ where: 'manualRoutes[0].target.port', message: 'want a whole number' }])
    expect(issues({ ...route, hostname: 3 })).toEqual([{ where: 'manualRoutes[0].hostname', message: 'want text' }])
    expect(issues({ ...route, options: { noTLSVerify: 1 } })).toEqual([{ where: 'manualRoutes[0].options.noTLSVerify', message: 'want true or false' }])
    expect(issues({ id: 'x' })).toEqual([
      { where: 'manualRoutes[0].hostname', message: 'missing' },
      { where: 'manualRoutes[0].target', message: 'missing' },
    ])
    expect(issues({ ...route, target: { kind: 'address' } })).toEqual([
      { where: 'manualRoutes[0].target.scheme', message: 'missing' },
      { where: 'manualRoutes[0].target.port', message: 'missing' },
    ])
  })

  test('a route without options has none', () => {
    expect(parseImportFile(file({ manualRoutes: [omit(routes[0] as ManualRouteView, 'options')] })).file?.routes?.[0]?.options).toEqual({})
  })
})

describe('what the daemon would refuse in a route, checked before anything is sent', () => {
  const cidrs = ['10.0.5.0/24']
  const base = routes[0] as ManualRouteView
  const edit = (over: Omit<Partial<ManualRouteView>, 'target'> & { target?: Partial<ManualTarget> }): ManualRouteView => ({
    ...base,
    ...over,
    target: { ...base.target, ...over.target },
  })
  const web: ManualRouteView['target'] = { kind: 'guest', scheme: 'https', guest: 'qemu/101', port: 8443 }
  const guestRoute = (options: ManualRouteView['options'] = {}) => ({ ...base, target: web, options })

  test('the route of the fixture is valid', () => {
    expect(validateRoute(base, cidrs)).toEqual([])
    expect(validateRoute(guestRoute({ noTLSVerify: true, sni: 'Web.Example.com', via: 'net1' }), cidrs)).toEqual([])
  })

  test.each<[string, string, ManualRouteView]>([
    ['an id with a capital', 'id', edit({ id: 'Status' })],
    ['an id with a dot', 'id', edit({ id: 'a.b' })],
    ['an empty id', 'id', edit({ id: '' })],
    ['an id of 33 characters', 'id', edit({ id: 'a'.repeat(33) })],
    ['a hostname of one label', 'hostname', edit({ hostname: 'localhost' })],
    ['a hostname with a space', 'hostname', edit({ hostname: 'a b.example.com' })],
    ['a scheme of ftp', 'target.scheme', edit({ target: { scheme: 'ftp' } })],
    ['port 0', 'target.port', edit({ target: { port: 0 } })],
    ['port 65536', 'target.port', edit({ target: { port: 65536 } })],
    ['a port with a fraction', 'target.port', edit({ target: { port: 80.5 } })],
    ['a kind of host', 'target.kind', edit({ target: { kind: 'host' } })],
    ['a guest of no kind', 'target.guest', { ...base, target: { ...web, guest: 'vm/101' } }],
    ['a guest with leading zeros', 'target.guest', { ...base, target: { ...web, guest: 'qemu/0101' } }],
    ['guest 0', 'target.guest', { ...base, target: { ...web, guest: 'qemu/0' } }],
    ['guest above 31 bits', 'target.guest', { ...base, target: { ...web, guest: 'qemu/2147483648' } }],
    ['an address with a guest', 'target.guest', edit({ target: { guest: 'qemu/101' } })],
    ['a guest with an address', 'target.addr', { ...base, target: { ...web, addr: '10.0.5.20' } }],
    ['an address of IPv6', 'target.addr', edit({ target: { addr: 'fd00::1' } })],
    ['an address with a prefix', 'target.addr', edit({ target: { addr: '10.0.5.0/24' } })],
    ['no address', 'target.addr', edit({ target: { addr: '' } })],
    ['an address outside the prefixes', 'target.addr', edit({ target: { addr: '10.0.6.20' } })],
    ['allowNode on a guest', 'options.allowNode', guestRoute({ allowNode: true })],
    ['noTLSVerify on http', 'options.noTLSVerify', edit({ options: { noTLSVerify: true } })],
    ['a host header with a slash', 'options.hostHeader', edit({ options: { hostHeader: 'a/b' } })],
    ['a host header over 253 characters', 'options.hostHeader', edit({ options: { hostHeader: 'a'.repeat(254) } })],
    ['sni on http', 'options.sni', edit({ options: { sni: 'a.example.com' } })],
    ['a wildcard sni', 'options.sni', guestRoute({ sni: '*.example.com' })],
    ['an sni that is no name', 'options.sni', guestRoute({ sni: 'a b' })],
    ['via on an address', 'options.via', edit({ options: { via: 'net1' } })],
    ['via net32', 'options.via', guestRoute({ via: 'net32' })],
    ['via net01', 'options.via', guestRoute({ via: 'net01' })],
    ['via net with no number', 'options.via', guestRoute({ via: 'net' })],
    ['via a loopback address', 'options.via', guestRoute({ via: '127.0.0.1' })],
    ['via a multicast address', 'options.via', guestRoute({ via: '224.0.0.1' })],
    ['via an address of 0.x', 'options.via', guestRoute({ via: '0.1.2.3' })],
    ['via a reserved address', 'options.via', guestRoute({ via: '240.0.0.1' })],
    ['via a link-local address', 'options.via', guestRoute({ via: '169.254.1.1' })],
    ['via a word', 'options.via', guestRoute({ via: 'eth0' })],
  ])('%s is refused at %s', (_name, field, r) => {
    expect(validateRoute(r, cidrs)[0]?.field).toBe(field)
  })

  test.each([
    ['via NET3', { via: 'NET3' }],
    ['via net0', { via: 'net0' }],
    ['via net31', { via: 'net31' }],
    ['via an address', { via: '10.0.5.9' }],
    ['a host header with a port', { hostHeader: 'app.example.com:8080' }],
    ['a host header with an underscore', { hostHeader: 'a_b.example.com' }],
  ])('%s is accepted', (_name, options) => {
    expect(validateRoute(guestRoute(options), cidrs)).toEqual([])
  })

  test('allowNode is accepted on an address, and does not lift the prefixes', () => {
    expect(validateRoute(edit({ options: { allowNode: true } }), cidrs)).toEqual([])
    expect(validateRoute(edit({ options: { allowNode: true }, target: { addr: '192.168.1.1' } }), cidrs)[0]?.field).toBe('target.addr')
  })

  test('the message for an address outside names the prefixes it was checked against', () => {
    expect(validateRoute(edit({ target: { addr: '10.0.6.20' } }), cidrs)[0]?.message).toBe(
      'target.addr 10.0.6.20: not inside the manualCIDRs of the imported settings (10.0.5.0/24)',
    )
    expect(validateRoute(edit({ target: { addr: '10.0.6.20' } }), [])[0]?.message).toBe(
      'target.addr 10.0.6.20: not inside the manualCIDRs of the imported settings (none)',
    )
    expect(validateRoute(edit({ target: { addr: '10.0.6.200' } }), ['10.0.5.0/24', '10.0.6.0/25'])[0]?.message).toContain('(10.0.5.0/24, 10.0.6.0/25)')
  })

  test('the routes are written in their normal form', () => {
    const got = normalizeRoute({
      ...base,
      hostname: 'Status.Example.COM.',
      target: { ...web, guest: 'qemu/101' },
      options: { sni: 'WEB.example.com', via: 'NET1', noTLSVerify: true, hostHeader: '', allowNode: false },
    })
    expect(got.hostname).toBe('status.example.com')
    expect(got.options).toEqual({ sni: 'web.example.com', via: 'net1', noTLSVerify: true })
    expect(got.target).toEqual(web)
  })
})

describe('each option is checked on its own, so that no import stops half done', () => {
  const https = (options: ManualRouteView['options']): ManualRouteView => ({
    id: 'web',
    rev: 1,
    hostname: 'web.example.com',
    target: { kind: 'guest', guest: 'qemu/101', scheme: 'https', port: 8443 },
    options,
  })

  test.each([
    [{ sni: 'x.example.com', via: 'net99' }, ['options.via']],
    [{ hostHeader: 'a b', sni: '*.example.com' }, ['options.hostHeader', 'options.sni']],
    [{ noTLSVerify: true, hostHeader: 'ok.example.com:8443', sni: 'bad_name', via: '127.0.0.1' }, ['options.sni', 'options.via']],
  ])('%j', (options, fields) => {
    expect(validateRoute(https(options), ['10.0.5.0/24']).map((i) => i.field)).toEqual(fields)
  })

  test('as the daemon quotes them', () => {
    expect(validateRoute(https({ via: 'net99' }), undefined).map((i) => i.message)).toEqual([
      'options.via: "net99" is neither a NIC from net0 to net31 nor an IPv4 address a guest can have',
    ])
  })
})

describe('everything is checked before anything is written', () => {
  test('a file that is as it should be has no issue', () => {
    const got = check(file())
    expect(got.issues).toEqual([])
    expect(got.file?.routes?.map((r) => r.id)).toEqual(['status'])
  })

  test('a setting out of its range is named, with the file path of it', () => {
    const got = check(file({ settings: { ...stored, pollInterval: '1s', cloudflareBudget: 2000 } }))
    expect(got.issues.map((i) => i.where)).toEqual(['settings.pollInterval', 'settings.cloudflareBudget'])
    expect(got.issues[0]?.message).toBe('pollInterval 1s: at least 5s')
  })

  test('leaving observe-only is refused, going back to it is not', () => {
    const observing = { ...stored, observeOnly: true }
    const leaving = check(file({ settings: { ...stored, observeOnly: false } }), observing)
    expect(leaving.issues.map((i) => i.where)).toEqual(['settings.observeOnly'])
    expect(leaving.issues[0]?.message).toContain('use apply')
    expect(check(file({ settings: observing }), stored).issues).toEqual([])
  })

  test('a route address is checked against the imported manualCIDRs, not the stored ones', () => {
    const elsewhere = { ...stored, manualCIDRs: ['192.168.9.0/24'] }
    // valid only under what the file brings
    expect(check(file(), elsewhere).issues).toEqual([])
    // and refused when the file brings others, though the stored ones would allow it
    const got = check(file({ settings: elsewhere }), stored)
    expect(got.issues).toEqual([
      {
        where: 'manualRoutes[0] (status)',
        message: 'target.addr 10.0.5.20: not inside the manualCIDRs of the imported settings (192.168.9.0/24)',
      },
    ])
  })

  test('an imported manualCIDRs that is invalid is one issue, not one for every route as well', () => {
    const got = check(file({ settings: { ...stored, manualCIDRs: ['fd00::/8'] } }))
    expect(got.issues.map((i) => i.where)).toEqual(['settings.manualCIDRs[0]'])
  })

  test('every route is named by its place and id, all of the problems at once', () => {
    const bad = [
      { ...routes[0], id: 'a', hostname: 'localhost' },
      { ...routes[0], id: 'b', target: { ...routes[0]?.target, port: 0 } },
      { ...routes[0], id: 'c' },
    ]
    const got = check(file({ manualRoutes: bad }))
    expect(got.issues.map((i) => i.where)).toEqual(['manualRoutes[0] (a)', 'manualRoutes[1] (b)'])
  })

  test('two routes of one id are refused: the second would overwrite the first', () => {
    const got = check(file({ manualRoutes: [routes[0], { ...routes[0], hostname: 'other.example.com' }] }))
    expect(got.issues).toEqual([{ where: 'manualRoutes[1] (status)', message: 'id status is the id of manualRoutes[0] too' }])
  })

  test('a route without an id cannot be set against the stored ones', () => {
    const got = check(file({ manualRoutes: [omit(routes[0] as ManualRouteView, 'id')] }))
    expect(got.issues).toEqual([{ where: 'manualRoutes[0].id', message: 'missing' }])
  })

  test('the routes come back in their normal form', () => {
    const upper = { ...routes[0], hostname: 'Status.Example.com' }
    expect(check(file({ manualRoutes: [upper] })).file?.routes?.[0]?.hostname).toBe('status.example.com')
  })
})
