import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { ApiError } from '../../api/client'
import { StoreProvider } from '../../api/store'
import type { RouteView, Settings, SettingsView } from '../../api/types.gen'
import { navigate } from '../../app/router'
import { ToastProvider } from '../../components/Toast'
import manualRoutes from '../../fixtures/manual-routes.json'
import populated from '../../fixtures/populated.json'
import settingsFixture from '../../fixtures/settings.json'
import { fakeStore } from '../../test/store'
import { fakeDaemon, type Handler, type Sent } from './fakeDaemon'
import { allowLink, SettingsPage } from './SettingsPage'

const view = settingsFixture as unknown as SettingsView

// A state with routes that wait for allowHosts, as the daemon says them.
const withRejected = (...routes: Partial<RouteView>[]) => ({
  ...populated,
  routes: [...populated.routes, ...routes.map((r) => ({ ...populated.routes[0], state: 'rejected', ...r }))],
})

afterEach(() => {
  vi.unstubAllGlobals()
})

interface Mount {
  url?: string
  view?: SettingsView
  state?: unknown
  role?: string
  handlers?: Record<string, Handler>
}

async function mount(o: Mount = {}) {
  navigate(o.url ?? '/settings')
  const daemon = fakeDaemon({
    'GET /api/v1/settings': () => ({ body: o.view ?? view }),
    'GET /api/v1/routes/manual': () => ({ body: manualRoutes }),
    ...o.handlers,
  })
  const { store } = await fakeStore({ state: o.state ?? populated, session: { role: o.role ?? 'admin' } })
  render(
    <StoreProvider store={store}>
      <ToastProvider>
        <SettingsPage />
      </ToastProvider>
    </StoreProvider>,
  )
  await screen.findByRole('form', { name: 'Settings' })
  return { daemon }
}

const saved =
  (over: Partial<SettingsView> & { restartNeeded?: string[] } = {}): Handler =>
  (s) => ({ body: { ...view, rev: 8, settings: (s.body as { settings: Settings }).settings, restartNeeded: [], ...over } })

const put = (daemon: { calls: Sent[] }) => daemon.calls.find((c) => c.method === 'PUT')?.body as { rev: number; settings: Settings } | undefined

const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } })

const save = () => fireEvent.click(screen.getByRole('button', { name: 'Save' }))

const describedBy = (control: HTMLElement) =>
  (control.getAttribute('aria-describedby') ?? '')
    .split(' ')
    .map((id) => document.getElementById(id)?.textContent ?? '')
    .join(' ')

describe('the settings issues', () => {
  test('the issues no guest has are listed first, as pco status prints them', async () => {
    await mount()
    expect(screen.getByRole('heading', { name: '1 issue with the settings' })).toBeTruthy()
    expect(screen.getByText('settings: an issue of the settings')).toBeTruthy()
    // an issue of the Notes of a guest is the Guests page's
    expect(screen.queryByText(/a broken entry/)).toBeNull()
    const issues = document.getElementById('settings-issues')?.closest('section')
    const form = screen.getByRole('form', { name: 'Settings' })
    expect(issues && form.compareDocumentPosition(issues) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy()
  })

  test('no issue, no card; the daemon notes on settings it uses otherwise are among them', async () => {
    await mount({
      state: { ...populated, issues: [] },
      view: { ...view, notes: ['settings: pollInterval is 1s in /etc/pve/pco/meta/settings.json, below the minimum of 5s; 5s is used until it is raised there'] },
    })
    expect(screen.getByRole('heading', { name: '1 issue with the settings' })).toBeTruthy()
    expect(screen.getByText(/pollInterval is 1s in \/etc\/pve\/pco\/meta\/settings\.json/)).toBeTruthy()
  })

  test('none to say, nothing shown', async () => {
    await mount({ state: { ...populated, issues: [] } })
    expect(screen.queryByText(/issues? with the settings/)).toBeNull()
  })
})

describe('the badge of the settings read at start', () => {
  const labels: Record<string, string> = {
    gateTag: 'Tag',
    trustStatic: 'Trust static addresses behind a router',
    trustedCIDRs: 'Prefixes of trusted static addresses',
    cloudflareBudget: 'Cloudflare budget',
    pollInterval: 'Poll interval',
    grace: 'Grace',
    reverifyInterval: 'Proof of identity stands for',
    maxHostnamesPerGuest: 'Hostnames per guest',
    allowHosts: 'Allowed hostnames',
    denyHosts: 'Denied hostnames',
    manualCIDRs: 'Prefixes of manual route addresses',
    zonePins: 'Zone pins',
    identityMinimum: 'Lowest identity level served',
  }
  const badged = () =>
    Object.keys(labels)
      .filter((name) => describedBy(screen.getByLabelText(labels[name] as string)).includes('read at start'))
      .sort()

  test('exactly the fields the daemon names, today the four of the fixture', async () => {
    await mount()
    expect(view.readAtStart).toContain('cloudflareBudget')
    expect(badged()).toEqual(['cloudflareBudget', 'gateTag', 'trustStatic', 'trustedCIDRs'])
  })

  test('never a list of the page: the daemon says another, the badge follows', async () => {
    await mount({ view: { ...view, readAtStart: ['pollInterval', 'zonePins'] } })
    expect(badged()).toEqual(['pollInterval', 'zonePins'])
  })

  test('none to say, none shown', async () => {
    await mount({ view: { ...view, readAtStart: [] } })
    expect(badged()).toEqual([])
  })

  test('the admission and the mode have one too when the daemon says so', async () => {
    await mount({ view: { ...view, readAtStart: ['admission', 'observeOnly'] } })
    expect(screen.getByRole('group', { name: /Which guests are published/ }).textContent).toContain('read at start')
    expect(screen.getByText(/pco publishes what it plans/).textContent).toContain('read at start')
    expect(badged()).toEqual([])
  })
})

describe('the ranges are the daemon words', () => {
  test('the hints of the timing and the limits say the limits of the answer and the defaults', async () => {
    await mount()
    expect(describedBy(screen.getByLabelText('Poll interval'))).toContain('At least 5s. Default 10s.')
    expect(describedBy(screen.getByLabelText('Grace'))).toContain('At least 30s. Default 1m0s.')
    expect(describedBy(screen.getByLabelText('Proof of identity stands for'))).toContain('From 10s to 5m0s. Default 1m0s.')
    expect(describedBy(screen.getByLabelText('Hostnames per guest'))).toContain('At least 1. Default 32.')
    expect(describedBy(screen.getByLabelText('Cloudflare budget'))).toContain('From 100 to 1150. Default 1000.')
  })

  test('another range is another hint, and another check', async () => {
    await mount({ view: { ...view, limits: { ...view.limits, cloudflareBudget: { min: 100, max: 500 } } } })
    expect(describedBy(screen.getByLabelText('Cloudflare budget'))).toContain('From 100 to 500.')
    type('Cloudflare budget', '600')
    expect(screen.getByText('cloudflareBudget 600: from 100 to 500')).toBeTruthy()
  })
})

describe('what is typed', () => {
  test('a mistake shows as it is typed, and a save is not asked while it stands', async () => {
    const { daemon } = await mount()
    const poll = screen.getByLabelText('Poll interval')
    expect(poll.getAttribute('aria-invalid')).toBeNull()
    type('Poll interval', '1s')
    expect(screen.getByText('pollInterval 1s: at least 5s')).toBeTruthy()
    expect(poll.getAttribute('aria-invalid')).toBe('true')
    expect(describedBy(poll)).toContain('pollInterval 1s: at least 5s')

    save()
    expect(screen.getByText('Fix the settings marked above before saving.')).toBeTruthy()
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(poll)
    expect(daemon.writes()).toEqual([])

    type('Poll interval', '15s')
    expect(screen.queryByText('pollInterval 1s: at least 5s')).toBeNull()
    expect(poll.getAttribute('aria-invalid')).toBeNull()
  })

  test('every mistake shows, and a save goes to the first of them', async () => {
    await mount()
    type('Tag', 'CF Tunnel')
    type('Denied hostnames', 'ok.example.com\nnot a host')
    save()
    expect(screen.getByText(/gateTag "CF Tunnel": want lower-case letters/)).toBeTruthy()
    expect(screen.getByText('denyHosts[1] "not": needs at least two labels')).toBeTruthy()
    expect(document.activeElement).toBe(screen.getByLabelText('Denied hostnames'))
  })

  test('nothing changed, nothing to save, and the reason is said', async () => {
    await mount()
    expect(screen.getByText('No changes to save')).toBeTruthy()
    save()
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})

describe('a save', () => {
  test('shows the diff first, and writes at the revision the settings were read at', async () => {
    const { daemon } = await mount({ handlers: { 'PUT /api/v1/settings': saved() } })
    type('Poll interval', '30s')
    type('Allowed hostnames', '*.example.com\nshop.cz')
    fireEvent.click(screen.getByRole('radio', { name: 'approve' }))
    save()

    const dialog = await screen.findByRole('dialog', { name: 'Save these changes?' })
    const rows = within(within(dialog).getByRole('table', { name: 'Changes to the settings' })).getAllByRole('row')
    expect(rows.map((r) => r.textContent)).toEqual([
      'SettingChange',
      'allowHostsadded *.example.comadded shop.cz',
      'pollInterval10s → becomes 30s',
      'admissiontag → becomes approve',
    ])
    expect(daemon.writes()).toEqual([])

    fireEvent.click(within(dialog).getByRole('button', { name: 'Save settings' }))
    expect(await screen.findByText('The settings are saved; they are at revision 8.')).toBeTruthy()
    expect(daemon.writes()).toEqual(['PUT /api/v1/settings'])
    expect(put(daemon)).toEqual({
      rev: 7,
      settings: { ...view.settings, pollInterval: '30s', allowHosts: ['*.example.com', 'shop.cz'], admission: 'approve' },
    })
    expect(screen.queryByRole('dialog')).toBeNull()
    // what is shown is what was saved, and there is nothing more to save
    expect((screen.getByLabelText('Poll interval') as HTMLInputElement).value).toBe('30s')
    expect(screen.getByText('No changes to save')).toBeTruthy()
  })

  test('the next save is at the revision of the last', async () => {
    const { daemon } = await mount({ handlers: { 'PUT /api/v1/settings': saved() } })
    type('Grace', '2m')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    await screen.findByText('The settings are saved; they are at revision 8.')
    type('Grace', '3m')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    await waitFor(() => expect(daemon.calls.filter((c) => c.method === 'PUT')).toHaveLength(2))
    expect((daemon.calls.filter((c) => c.method === 'PUT')[1]?.body as { rev: number }).rev).toBe(8)
  })

  test('cancelling the diff writes nothing and keeps the edits', async () => {
    const { daemon } = await mount()
    type('Grace', '2m')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(daemon.writes()).toEqual([])
    expect((screen.getByLabelText('Grace') as HTMLInputElement).value).toBe('2m')
    fireEvent.click(screen.getByRole('button', { name: 'Discard changes' }))
    expect((screen.getByLabelText('Grace') as HTMLInputElement).value).toBe('1m0s')
  })

  test('what the daemon refuses is said at the field, in its words, whatever the page thought', async () => {
    const { daemon } = await mount({
      // the page has no range for the poll interval, so it lets 1s through
      view: { ...view, limits: {} },
      handlers: {
        'PUT /api/v1/settings': () => ({ status: 400, body: { code: 'invalid', error: 'pollInterval 1s: at least 5s', field: 'pollInterval' } }),
      },
    })
    type('Poll interval', '1s')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    expect(await screen.findByText('pollInterval 1s: at least 5s')).toBeTruthy()
    expect(screen.getByLabelText('Poll interval').getAttribute('aria-invalid')).toBe('true')
    expect(document.activeElement).toBe(screen.getByLabelText('Poll interval'))
    expect(daemon.writes()).toEqual(['PUT /api/v1/settings'])
    // typing again clears it
    type('Poll interval', '12s')
    expect(screen.queryByText('pollInterval 1s: at least 5s')).toBeNull()
  })

  test('an invalid answer for a path of a list is shown at the list', async () => {
    await mount({
      handlers: { 'PUT /api/v1/settings': () => ({ status: 400, body: { code: 'invalid', error: 'denyHosts[1] "x.y": nope', field: 'denyHosts[1]' } }) },
    })
    type('Denied hostnames', 'a.example.com\nb.example.com')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    expect(await screen.findByText('denyHosts[1] "x.y": nope')).toBeTruthy()
    expect(screen.getByLabelText('Denied hostnames').getAttribute('aria-invalid')).toBe('true')
  })

  test('another failure is a toast, and the edits stay', async () => {
    await mount({ handlers: { 'PUT /api/v1/settings': () => ({ status: 503, body: { code: 'unavailable', error: 'the daemon is busy' } }) } })
    type('Grace', '2m')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    expect(await screen.findByText('the daemon is busy')).toBeTruthy()
    expect((screen.getByLabelText('Grace') as HTMLInputElement).value).toBe('2m')
  })

  test('someone saved in between: both versions are shown, and the edits go on the new ones', async () => {
    let now: SettingsView = view
    const theirs: SettingsView = { ...view, rev: 8, settings: { ...view.settings, pollInterval: '30s', cloudflareBudget: 700 } }
    const { daemon } = await mount({
      handlers: {
        'GET /api/v1/settings': () => ({ body: now }),
        'PUT /api/v1/settings': (s) => {
          if ((s.body as { rev: number }).rev === 7) {
            now = theirs
            return { status: 409, body: { code: 'refused', error: 'refused: the settings changed since they were read at revision 7 and are at revision 8 now; read them again' } }
          }
          return saved({ rev: 9 })(s)
        },
      },
    })
    type('Grace', '2m')
    type('Cloudflare budget', '500')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))

    const dialog = await screen.findByRole('dialog', { name: 'The settings changed while you edited' })
    expect(within(dialog).getByText(/the settings changed since they were read at revision 7 and are at revision 8 now/)).toBeTruthy()
    const others = within(dialog).getByRole('table', { name: 'Saved since the page was opened' })
    expect(within(others).getByText('pollInterval')).toBeTruthy()
    expect(within(others).getByText('cloudflareBudget')).toBeTruthy()
    expect(within(others).queryByText('grace')).toBeNull()
    const mine = within(dialog).getByRole('table', { name: 'Your edits' })
    expect(within(mine).getByText('grace')).toBeTruthy()
    expect(within(mine).getByText('cloudflareBudget')).toBeTruthy()

    fireEvent.click(within(dialog).getByRole('button', { name: 'Keep my edits' }))
    // a setting I did not touch is theirs, one I edited is mine
    expect((screen.getByLabelText('Poll interval') as HTMLInputElement).value).toBe('30s')
    expect((screen.getByLabelText('Grace') as HTMLInputElement).value).toBe('2m')
    expect((screen.getByLabelText('Cloudflare budget') as HTMLInputElement).value).toBe('500')

    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    await waitFor(() => expect(daemon.calls.filter((c) => c.method === 'PUT')).toHaveLength(2))
    const second = daemon.calls.filter((c) => c.method === 'PUT')[1]?.body as { rev: number; settings: Settings }
    expect(second.rev).toBe(8)
    expect(second.settings).toMatchObject({ pollInterval: '30s', grace: '2m', cloudflareBudget: 500 })
  })

  test('someone saved in between: the saved settings can be taken instead', async () => {
    let now: SettingsView = view
    const theirs: SettingsView = { ...view, rev: 8, settings: { ...view.settings, pollInterval: '30s' } }
    await mount({
      handlers: {
        'GET /api/v1/settings': () => ({ body: now }),
        'PUT /api/v1/settings': () => {
          now = theirs
          return { status: 409, body: { code: 'refused', error: 'refused: the settings changed' } }
        },
      },
    })
    type('Grace', '2m')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Use the saved settings' }))
    expect((screen.getByLabelText('Poll interval') as HTMLInputElement).value).toBe('30s')
    expect((screen.getByLabelText('Grace') as HTMLInputElement).value).toBe('1m0s')
    expect(screen.getByText('No changes to save')).toBeTruthy()
  })
})

describe('the settings read at start', () => {
  test('the diff says which of the changes wait for a restart', async () => {
    await mount({ handlers: { 'PUT /api/v1/settings': saved() } })
    type('Tag', 'pco')
    type('Grace', '2m')
    save()
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('gateTag is read only when pco starts: it takes effect after systemctl restart pco.')).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    fireEvent.click(screen.getByLabelText('Trust static addresses behind a router'))
    save()
    expect(within(await screen.findByRole('dialog')).getByText('gateTag, trustStatic are read only when pco starts: they take effect after systemctl restart pco.')).toBeTruthy()
  })

  test('after the save the page says so and offers the restart, which asks first', async () => {
    const { daemon } = await mount({
      handlers: {
        'PUT /api/v1/settings': saved({ restartNeeded: ['gateTag'] }),
        'POST /api/v1/daemon/restart': () => ({ status: 202 }),
      },
    })
    type('Tag', 'pco')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    await screen.findByText('The settings are saved; they are at revision 8.')
    expect(screen.getByText(/is read only when pco starts: it takes effect after/)).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Restart pco…' }))
    const dialog = await screen.findByRole('dialog', { name: 'Restart the daemon?' })
    expect(within(dialog).getByText(/The connectors keep running/)).toBeTruthy()
    expect(daemon.writes()).toEqual(['PUT /api/v1/settings'])
    fireEvent.click(within(dialog).getByRole('button', { name: 'Restart' }))
    expect(await screen.findByText(/The daemon restarts/)).toBeTruthy()
    expect(daemon.writes()).toEqual(['PUT /api/v1/settings', 'POST /api/v1/daemon/restart'])
    expect(screen.queryByRole('button', { name: 'Restart pco…' })).toBeNull()
  })

  test('a daemon that cannot restart itself says so, and the notice stays', async () => {
    await mount({
      handlers: {
        'PUT /api/v1/settings': saved({ restartNeeded: ['trustStatic', 'gateTag'] }),
        'POST /api/v1/daemon/restart': () => ({ status: 409, body: { code: 'refused', error: 'refused: this daemon cannot restart itself; run systemctl restart pco' } }),
      },
    })
    fireEvent.click(screen.getByLabelText('Trust static addresses behind a router'))
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Restart pco…' }))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Restart' }))
    expect(await screen.findByText(/this daemon cannot restart itself/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Restart pco…' })).toBeTruthy()
    expect(screen.getByText(/are read only when pco starts: they take effect after/)).toBeTruthy()
  })

  test('a save that needs no restart shows no notice', async () => {
    await mount({ handlers: { 'PUT /api/v1/settings': saved() } })
    type('Grace', '2m')
    save()
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save settings' }))
    await screen.findByText('The settings are saved; they are at revision 8.')
    expect(screen.queryByRole('button', { name: 'Restart pco…' })).toBeNull()
  })
})

describe('publishing', () => {
  test('in observe-only the way out is the plan, not a setting', async () => {
    await mount({ view: { ...view, settings: { ...view.settings, observeOnly: true } } })
    expect(screen.getByRole('link', { name: 'Review the plan and start publishing' }).getAttribute('href')).toBe('/routes/plan')
    expect(screen.queryByRole('button', { name: 'Return to observe-only' })).toBeNull()
  })

  test('publishing can go back to observe-only, which is a setting', async () => {
    const { daemon } = await mount({ handlers: { 'PUT /api/v1/settings': saved() } })
    expect(screen.queryByRole('link', { name: 'Review the plan and start publishing' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Return to observe-only' }))
    expect(screen.getByRole('button', { name: 'Return to observe-only' }).getAttribute('aria-pressed')).toBe('true')
    save()
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/After this save pco only observes/)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save settings' }))
    await screen.findByText('The settings are saved; they are at revision 8.')
    expect(put(daemon)?.settings.observeOnly).toBe(true)
  })
})

describe('a link from a route that waits for allowHosts', () => {
  const rejected = withRejected({ hostname: 'example.com', owner: 'qemu/102', reason: 'the apex needs allowHosts' })
  const allowed = () => (screen.getByLabelText('Allowed hostnames') as HTMLTextAreaElement).value

  test('is read only for a rejected route of that owner', () => {
    const routes = rejected.routes as unknown as RouteView[]
    expect(allowLink('?addAllowHost=example.com&owner=qemu%2F102', routes)).toEqual({ kind: 'accepted', request: { pattern: 'example.com', owner: 'qemu/102' } })
    expect(allowLink('', routes)).toEqual({ kind: 'none' })
    expect(allowLink('?owner=qemu%2F102', routes)).toEqual({ kind: 'none' })
    expect(allowLink('?addAllowHost=example.com&owner=qemu%2F102', undefined)).toEqual({ kind: 'wait' })
    for (const search of [
      '?addAllowHost=example.com',
      '?addAllowHost=example.com&owner=qemu%2F101',
      '?addAllowHost=other.example.com&owner=qemu%2F102',
      '?addAllowHost=www.example.com&owner=qemu%2F101',
      '?addAllowHost=*&owner=qemu%2F102',
      '?addAllowHost=&owner=qemu%2F102',
    ]) {
      expect(allowLink(search, routes), search).toEqual({ kind: 'refused' })
    }
  })

  test('adds the pattern to the form, focuses the list and saves nothing', async () => {
    const { daemon } = await mount({ url: '/settings?addAllowHost=example.com&owner=qemu%2F102', state: rejected })
    expect(allowed()).toBe('example.com')
    expect(document.activeElement).toBe(screen.getByLabelText('Allowed hostnames'))
    expect(screen.getByText(/is in the allowed hostnames below, for the route of/)).toBeTruthy()
    expect(screen.getByText(/Nothing is saved until you press Save/)).toBeTruthy()
    expect(daemon.writes()).toEqual([])
  })

  test('keeps the patterns that are there', async () => {
    await mount({
      url: '/settings?addAllowHost=example.com&owner=qemu%2F102',
      state: rejected,
      view: { ...view, settings: { ...view.settings, allowHosts: ['*.shop.cz'] } },
    })
    expect(allowed()).toBe('*.shop.cz\nexample.com')
  })

  test('the diff names the owner and what the opt-in grants, and Save writes it', async () => {
    const { daemon } = await mount({
      url: '/settings?addAllowHost=example.com&owner=qemu%2F102',
      state: rejected,
      handlers: { 'PUT /api/v1/settings': saved() },
    })
    save()
    const dialog = await screen.findByRole('dialog', { name: 'Save these changes?' })
    expect(within(dialog).getByRole('table', { name: 'Changes to the settings' }).textContent).toContain('example.com')
    expect(within(dialog).getByText(/example\.com is asked for by qemu\/102\./).textContent).toContain(
      'Whoever may edit the Notes of qemu/102, or of any other tagged guest, may then publish example.com, the apex of its zone, with a valid certificate.',
    )
    expect(daemon.writes()).toEqual([])
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save settings' }))
    await screen.findByText('The settings are saved; they are at revision 8.')
    expect(put(daemon)?.settings.allowHosts).toEqual(['example.com'])
  })

  test('a wildcard grants the names that have no record of their own', async () => {
    await mount({
      url: `/settings?addAllowHost=${encodeURIComponent('*.example.com')}&owner=lxc%2F200`,
      state: withRejected({ hostname: '*.example.com', owner: 'lxc/200' }),
    })
    expect(allowed()).toBe('*.example.com')
    save()
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/is asked for by lxc\/200/).textContent).toContain(
      'the wildcard that answers every name below example.com that has no record of its own, with a valid certificate',
    )
  })

  test('a pattern that is edited out of the list is no opt-in of the save', async () => {
    await mount({ url: '/settings?addAllowHost=example.com&owner=qemu%2F102', state: rejected })
    type('Allowed hostnames', 'other.example.com')
    save()
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).queryByText(/is asked for by/)).toBeNull()
  })

  test('a link that names no rejected route is ignored, and says so', async () => {
    const { daemon } = await mount({ url: '/settings?addAllowHost=evil.example.com&owner=qemu%2F102', state: rejected })
    expect(screen.getByText('This link names no rejected route, so nothing was added to the allowed hostnames.')).toBeTruthy()
    expect(allowed()).toBe('')
    expect(daemon.writes()).toEqual([])
  })

  test('the hostname of a route that is not rejected is no pattern to add', async () => {
    await mount({ url: '/settings?addAllowHost=www.example.com&owner=qemu%2F101' })
    expect(screen.getByText(/This link names no rejected route/)).toBeTruthy()
    expect(allowed()).toBe('')
  })

  test('a reader reads the settings and adds nothing', async () => {
    await mount({ url: '/settings?addAllowHost=example.com&owner=qemu%2F102', state: rejected, role: 'reader' })
    expect(allowed()).toBe('')
    expect(screen.queryByText(/This link names no rejected route/)).toBeNull()
  })

  test('waits for the state to tell the link from a mistake', async () => {
    navigate('/settings?addAllowHost=example.com&owner=qemu%2F102')
    fakeDaemon({ 'GET /api/v1/settings': () => ({ body: view }), 'GET /api/v1/routes/manual': () => ({ body: [] }) })
    const { store } = await fakeStore({
      state: rejected,
      answers: { 'GET /api/v1/state': () => new ApiError(503, { code: 'unavailable', error: 'no state yet' }) },
    })
    render(
      <StoreProvider store={store}>
        <ToastProvider>
          <SettingsPage />
        </ToastProvider>
      </StoreProvider>,
    )
    expect(await screen.findByText('Loading the settings')).toBeTruthy()
    expect(screen.queryByRole('form', { name: 'Settings' })).toBeNull()
    expect(screen.queryByText(/This link names no rejected route/)).toBeNull()
  })
})

describe('a reader', () => {
  test('reads every setting, changes none, and still exports', async () => {
    await mount({ role: 'reader' })
    expect(screen.getByText('Only admins can change the settings. You can read them here and export them below.')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull()
    expect(screen.getByLabelText('Poll interval').closest('fieldset')?.disabled).toBe(true)
    expect((screen.getByLabelText('Poll interval') as HTMLInputElement).value).toBe('10s')
    expect(screen.getByRole('button', { name: 'Export' })).toBeTruthy()
    expect(screen.getByText('Only admins can import')).toBeTruthy()
  })
})

describe('loading', () => {
  test('an answer that fails says why and tries again when asked', async () => {
    navigate('/settings')
    let answer = 0
    fakeDaemon({
      'GET /api/v1/settings': () => (++answer === 1 ? { status: 503, body: { code: 'unavailable', error: 'the daemon is starting' } } : { body: view }),
      'GET /api/v1/routes/manual': () => ({ body: [] }),
    })
    const { store } = await fakeStore({ state: populated })
    render(
      <StoreProvider store={store}>
        <ToastProvider>
          <SettingsPage />
        </ToastProvider>
      </StoreProvider>,
    )
    expect(await screen.findByText('the daemon is starting')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('form', { name: 'Settings' })).toBeTruthy()
  })
})
