import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import type { ManualRouteView, Settings, SettingsView } from '../../api/types.gen'
import { ToastProvider } from '../../components/Toast'
import manualFixture from '../../fixtures/manual-routes.json'
import settingsFixture from '../../fixtures/settings.json'
import { ExportImport, exportName, exportText } from './ExportImport'
import { fakeDaemon, type Handler } from './fakeDaemon'

const stored: Settings = { ...(settingsFixture.settings as unknown as Settings), manualCIDRs: ['10.0.5.0/24'] }
const view: SettingsView = { ...(settingsFixture as unknown as SettingsView), settings: stored }
const status = (manualFixture as unknown as ManualRouteView[])[0] as ManualRouteView
const old: ManualRouteView = { ...status, id: 'old', rev: 3, hostname: 'old.example.com' }
const web: ManualRouteView = {
  id: 'web',
  rev: 1,
  hostname: 'web.example.com',
  target: { kind: 'guest', guest: 'qemu/101', scheme: 'https', port: 8443 },
  options: { noTLSVerify: true },
}
const saved = [status, old]

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

interface Mount {
  canWrite?: boolean
  view?: SettingsView
  routes?: ManualRouteView[]
  handlers?: Record<string, Handler>
}

function mount(o: Mount = {}) {
  const daemon = fakeDaemon({
    'GET /api/v1/settings': () => ({ body: o.view ?? view }),
    'GET /api/v1/routes/manual': () => ({ body: o.routes ?? saved }),
    'PUT /api/v1/settings': (s) => ({ body: { ...view, rev: 8, settings: (s.body as { settings: Settings }).settings, restartNeeded: [] } }),
    'POST /api/v1/routes/manual': (s) => ({ status: 201, body: { ...(s.body as object), rev: 1 } }),
    'PUT /api/v1/routes/manual/status': (s) => ({ body: { ...(s.body as object), rev: 3 } }),
    'DELETE /api/v1/routes/manual/old': () => ({ body: {} }),
    ...o.handlers,
  })
  const imported = vi.fn()
  render(
    <ToastProvider>
      <ExportImport node="pve1" canWrite={o.canWrite ?? true} onImported={imported} now={() => new Date(2026, 9, 6, 14, 30)} />
    </ToastProvider>,
  )
  return { daemon, imported }
}

const fileOf = (content: unknown) =>
  new File([typeof content === 'string' ? content : JSON.stringify(content)], 'settings.json', { type: 'application/json' })

function choose(file: File) {
  fireEvent.change(screen.getByLabelText('Settings file'), { target: { files: [file] } })
}

const wanted = (over: Partial<Settings> = {}, routes: ManualRouteView[] | null = [status, web]) => ({
  rev: 3,
  settings: { ...stored, ...over },
  ...(routes ? { manualRoutes: routes } : {}),
})

describe('the file name and the file', () => {
  test('the name has the node and the date of the browser', () => {
    expect(exportName('pve1', new Date(2026, 9, 6, 23, 59))).toBe('pco-settings-pve1-20261006.json')
    expect(exportName('pve1', new Date(2026, 0, 2))).toBe('pco-settings-pve1-20260102.json')
    expect(exportName('node/with space', new Date(2026, 9, 6))).toBe('pco-settings-node-with-space-20261006.json')
    expect(exportName('', new Date(2026, 9, 6))).toBe('pco-settings-node-20261006.json')
  })

  test('the file is the revision, the settings and the routes', () => {
    const text = exportText(view, saved)
    expect(JSON.parse(text)).toEqual({ rev: 7, settings: stored, manualRoutes: saved })
    expect(Object.keys(JSON.parse(text) as object)).toEqual(['rev', 'settings', 'manualRoutes'])
    expect(text.endsWith('\n')).toBe(true)
  })
})

describe('export', () => {
  let blobs: Blob[]
  let downloads: { name: string; href: string }[]

  beforeEach(() => {
    blobs = []
    downloads = []
    vi.stubGlobal('URL', {
      ...URL,
      createObjectURL: (b: Blob) => {
        blobs.push(b)
        return `blob:test/${blobs.length}`
      },
      revokeObjectURL: () => {},
    })
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      downloads.push({ name: this.download, href: this.href })
    })
  })

  test('downloads the settings and the routes as the daemon has them now', async () => {
    const { daemon } = mount()
    fireEvent.click(screen.getByRole('button', { name: 'Export' }))
    await waitFor(() => expect(downloads).toHaveLength(1))
    expect(downloads[0]?.name).toBe('pco-settings-pve1-20261006.json')
    expect(downloads[0]?.href).toBe('blob:test/1')
    expect(JSON.parse(await (blobs[0] as Blob).text())).toEqual({ rev: 7, settings: stored, manualRoutes: saved })
    expect(blobs[0]?.type).toBe('application/json')
    expect(daemon.writes()).toEqual([])
  })

  test('a reader may export', async () => {
    mount({ canWrite: false })
    fireEvent.click(screen.getByRole('button', { name: 'Export' }))
    await waitFor(() => expect(downloads).toHaveLength(1))
  })

  test('a daemon that does not answer is said, and nothing is downloaded', async () => {
    mount({ handlers: { 'GET /api/v1/routes/manual': () => ({ status: 503, body: { code: 'unavailable', error: 'the daemon is busy' } }) } })
    fireEvent.click(screen.getByRole('button', { name: 'Export' }))
    expect(await screen.findByText('the daemon is busy')).toBeTruthy()
    expect(downloads).toEqual([])
  })
})

describe('a file that is wrong writes nothing', () => {
  test('a setting out of its range, named with its path in the file', async () => {
    const { daemon } = mount()
    choose(fileOf(wanted({ pollInterval: '1s', cloudflareBudget: 5000 })))
    const dialog = await screen.findByRole('dialog', { name: 'This file cannot be imported' })
    expect(within(dialog).getByText('settings.pollInterval')).toBeTruthy()
    expect(within(dialog).getByText('pollInterval 1s: at least 5s')).toBeTruthy()
    expect(within(dialog).getByText('settings.cloudflareBudget')).toBeTruthy()
    expect(within(dialog).getByText(/Nothing was written/)).toBeTruthy()
    expect(daemon.writes()).toEqual([])
    expect(within(dialog).queryByRole('button', { name: 'Import' })).toBeNull()
  })

  test('a route that is invalid refuses the whole file, the settings with it', async () => {
    const { daemon } = mount()
    choose(fileOf(wanted({ pollInterval: '30s' }, [status, { ...web, target: { ...web.target, port: 0 } }])))
    const dialog = await screen.findByRole('dialog', { name: 'This file cannot be imported' })
    expect(within(dialog).getByText('manualRoutes[1] (web)')).toBeTruthy()
    expect(within(dialog).getByText('target.port: want a port from 1 to 65535')).toBeTruthy()
    expect(daemon.writes()).toEqual([])
  })

  test('a route address that only the stored prefixes allow is refused: the file brings its own', async () => {
    const { daemon } = mount()
    choose(fileOf(wanted({ manualCIDRs: ['192.168.9.0/24'] })))
    const dialog = await screen.findByRole('dialog', { name: 'This file cannot be imported' })
    expect(within(dialog).getByText('target.addr 10.0.5.20: not inside the manualCIDRs of the imported settings (192.168.9.0/24)')).toBeTruthy()
    expect(daemon.writes()).toEqual([])
  })

  test('leaving observe-only is refused with the daemon words', async () => {
    mount({ view: { ...view, settings: { ...stored, observeOnly: true } } })
    choose(fileOf(wanted({ observeOnly: false })))
    const dialog = await screen.findByRole('dialog', { name: 'This file cannot be imported' })
    expect(within(dialog).getByText('settings.observeOnly')).toBeTruthy()
    expect(within(dialog).getByText(/use apply/)).toBeTruthy()
  })

  test('text that is no JSON, and a file too large', async () => {
    const { daemon } = mount()
    choose(fileOf('{"settings": '))
    expect(within(await screen.findByRole('dialog')).getByText('this is not JSON')).toBeTruthy()
    fireEvent.click(screen.getByText('Close'))
    choose(fileOf('x'.repeat((1 << 20) + 1)))
    expect(within(await screen.findByRole('dialog')).getByText('the file is larger than 1048576 bytes')).toBeTruthy()
    expect(daemon.writes()).toEqual([])
  })
})

describe('the diff of a file that is right', () => {
  test('a merge adds and updates by id and deletes nothing; a replace lists what it deletes by name', async () => {
    const { daemon } = mount()
    choose(fileOf(wanted({ pollInterval: '30s' }, [{ ...status, target: { ...status.target, port: 9001 } }, web])))
    const dialog = await screen.findByRole('dialog', { name: 'Import this file?' })
    expect(within(dialog).getByText(/Nothing has been written yet/)).toBeTruthy()
    expect(within(within(dialog).getByRole('table', { name: 'Changes to the settings' })).getByText('pollInterval')).toBeTruthy()

    expect((within(dialog).getByRole('radio', { name: 'Merge' }) as HTMLInputElement).checked).toBe(true)
    expect(within(dialog).getByRole('heading', { name: 'Added (1)' })).toBeTruthy()
    expect(within(dialog).getByText('web: web.example.com → https://qemu/101:8443')).toBeTruthy()
    expect(within(dialog).getByRole('heading', { name: 'Updated (1)' })).toBeTruthy()
    expect(within(dialog).getByText('status: target http://10.0.5.20:9000 → http://10.0.5.20:9001')).toBeTruthy()
    expect(within(dialog).queryByRole('heading', { name: /Deleted/ })).toBeNull()

    fireEvent.click(within(dialog).getByRole('radio', { name: 'Replace' }))
    expect(within(dialog).getByRole('heading', { name: 'Deleted (1)' })).toBeTruthy()
    expect(within(dialog).getByText('old: old.example.com → http://10.0.5.20:9000')).toBeTruthy()
    expect(daemon.writes()).toEqual([])
  })

  test('routes that are as saved are counted, not listed', async () => {
    mount()
    choose(fileOf(wanted({ pollInterval: '30s' }, [status, old])))
    const dialog = await screen.findByRole('dialog', { name: 'Import this file?' })
    expect(within(dialog).getByText('2 routes are the same as saved.')).toBeTruthy()
    expect(within(dialog).queryByRole('heading', { name: /Added|Updated/ })).toBeNull()
  })

  test('a file that is what is saved has nothing to import', async () => {
    const { daemon } = mount()
    choose(fileOf(wanted({}, saved)))
    const dialog = await screen.findByRole('dialog', { name: 'Import this file?' })
    expect(within(dialog).getByText('There is nothing to import: the file matches what is saved.')).toBeTruthy()
    expect(within(dialog).queryByRole('button', { name: 'Import' })).toBeNull()
    expect(daemon.writes()).toEqual([])
  })

  test('a file with no manual routes leaves those saved alone, and offers no replace', async () => {
    mount()
    choose(fileOf(wanted({ pollInterval: '30s' }, null)))
    const dialog = await screen.findByRole('dialog', { name: 'Import this file?' })
    expect(within(dialog).getByText('The file has no manual routes; those saved stay as they are.')).toBeTruthy()
    expect(within(dialog).queryByRole('radio')).toBeNull()
  })

  test('what pco settings show --json prints is a file as well', async () => {
    mount()
    choose(fileOf({ ...settingsFixture, settings: { ...stored, grace: '2m' } }))
    const dialog = await screen.findByRole('dialog', { name: 'Import this file?' })
    expect(within(dialog).getByText('grace')).toBeTruthy()
  })
})

describe('the writes of an import', () => {
  test('the settings first, then the routes added, changed and deleted, each at the revision stored', async () => {
    const { daemon, imported } = mount({
      handlers: { 'PUT /api/v1/settings': (s) => ({ body: { ...view, rev: 8, settings: (s.body as { settings: Settings }).settings, restartNeeded: ['gateTag'] } }) },
    })
    choose(fileOf(wanted({ pollInterval: '30s', gateTag: 'pco' }, [{ ...status, rev: 99, target: { ...status.target, port: 9001 } }, web])))
    const dialog = await screen.findByRole('dialog', { name: 'Import this file?' })
    fireEvent.click(within(dialog).getByRole('radio', { name: 'Replace' }))
    expect(daemon.writes()).toEqual([])
    fireEvent.click(within(dialog).getByRole('button', { name: 'Import' }))

    const done = await screen.findByRole('dialog', { name: 'Import' })
    expect(within(done).getByText('Everything of the file is saved.')).toBeTruthy()
    expect(daemon.writes()).toEqual([
      'PUT /api/v1/settings',
      'POST /api/v1/routes/manual',
      'PUT /api/v1/routes/manual/status',
      'DELETE /api/v1/routes/manual/old?rev=3',
    ])
    const [settingsCall, createCall, updateCall] = daemon.calls.filter((c) => c.method !== 'GET')
    expect(settingsCall?.body).toEqual({ rev: 7, settings: { ...stored, pollInterval: '30s', gateTag: 'pco' } })
    expect(createCall?.body).toEqual({ id: 'web', hostname: 'web.example.com', target: web.target, options: web.options })
    // the revision of the route as stored, not the one the file carries
    expect((updateCall?.body as { rev: number }).rev).toBe(2)
    expect(within(done).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['settings', 'add route web', 'change route status', 'delete route old'])
    expect(within(done).getByText('gateTag take effect after systemctl restart pco.')).toBeTruthy()
    expect(imported).toHaveBeenCalledWith(['gateTag'])
  })

  test('settings that did not change are not saved again', async () => {
    const { daemon } = mount()
    choose(fileOf(wanted({}, [status, web])))
    fireEvent.click(within(await screen.findByRole('dialog', { name: 'Import this file?' })).getByRole('button', { name: 'Import' }))
    await screen.findByRole('dialog', { name: 'Import' })
    expect(daemon.writes()).toEqual(['POST /api/v1/routes/manual'])
  })

  test('a route address valid only under the prefixes of the file: those are saved first', async () => {
    const { daemon } = mount({ view: { ...view, settings: { ...stored, manualCIDRs: [] } } })
    const next = { ...status, id: 'status2', hostname: 'status2.example.com', target: { ...status.target, addr: '10.0.5.21' } }
    choose(fileOf(wanted({ manualCIDRs: ['10.0.5.0/24'] }, [status, next])))
    fireEvent.click(within(await screen.findByRole('dialog', { name: 'Import this file?' })).getByRole('button', { name: 'Import' }))
    await screen.findByRole('dialog', { name: 'Import' })
    expect(daemon.writes()).toEqual(['PUT /api/v1/settings', 'POST /api/v1/routes/manual'])
  })

  test('the first refusal stops it: what was saved and what was not are listed', async () => {
    const { daemon, imported } = mount({
      handlers: {
        'POST /api/v1/routes/manual': () => ({
          status: 409,
          body: { code: 'refused', error: 'refused: the id web is taken by manual/web; choose another one' },
        }),
      },
    })
    choose(fileOf(wanted({ pollInterval: '30s' }, [{ ...status, target: { ...status.target, port: 9001 } }, web])))
    const dialog = await screen.findByRole('dialog', { name: 'Import this file?' })
    fireEvent.click(within(dialog).getByRole('radio', { name: 'Replace' }))
    fireEvent.click(within(dialog).getByRole('button', { name: 'Import' }))

    const done = await screen.findByRole('dialog', { name: 'Import' })
    expect(within(done).getByRole('alert').textContent).toBe('Stopped at add route web: refused: the id web is taken by manual/web; choose another one')
    const saved = within(done).getByRole('heading', { name: 'Saved' }).nextElementSibling
    expect(Array.from(saved?.querySelectorAll('li') ?? []).map((li) => li.textContent)).toEqual(['settings'])
    const notSaved = within(done).getByRole('heading', { name: 'Not saved' }).nextElementSibling
    expect(Array.from(notSaved?.querySelectorAll('li') ?? []).map((li) => li.textContent)).toEqual(['change route status', 'delete route old'])
    expect(daemon.writes()).toEqual(['PUT /api/v1/settings', 'POST /api/v1/routes/manual'])
    // what was saved is shown again
    expect(imported).toHaveBeenCalledTimes(1)
  })

  test('the settings refused: no route is tried', async () => {
    const { daemon } = mount({
      handlers: { 'PUT /api/v1/settings': () => ({ status: 400, body: { code: 'invalid', error: 'identityMinimum "x": want one of them', field: 'identityMinimum' } }) },
    })
    choose(fileOf(wanted({ pollInterval: '30s' })))
    fireEvent.click(within(await screen.findByRole('dialog', { name: 'Import this file?' })).getByRole('button', { name: 'Import' }))
    const done = await screen.findByRole('dialog', { name: 'Import' })
    expect(within(done).getByRole('alert').textContent).toContain('Stopped at settings')
    expect(within(done).queryByRole('heading', { name: 'Saved' })).toBeNull()
    expect(within(done).getByRole('heading', { name: 'Not saved' })).toBeTruthy()
    expect(daemon.writes()).toEqual(['PUT /api/v1/settings'])
  })
})

describe('a reader', () => {
  test('does not import', () => {
    mount({ canWrite: false })
    expect(screen.getByText('Only admins can import')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Export' })).toBeTruthy()
  })
})
