import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../../api/store'
import type { CredentialView, RouteView, State, ZoneView } from '../../api/types.gen'
import { Page } from '../../app/Page'
import { match, navigate, useView } from '../../app/router'
import { ToastProvider } from '../../components/Toast'
import firstRun from '../../fixtures/first-run.json'
import populated from '../../fixtures/populated.json'
import { fakeStore, flush } from '../../test/store'
import { stubApi, writes } from '../testing'
import { applying, applyingAlways } from '../routes/PlanPage'
import { dismissedKey, setDismissed } from './dismissed'
import { accountTokenUrl, userTokenUrl } from './tokenTemplate'
import { SetupPage, Start } from './Wizard'

const fresh = firstRun as unknown as State
const full = populated as unknown as State
const good = full.credentials[0] as CredentialView
const www = full.routes[0] as RouteView
const token = 'Ab3_dEf-0123456789ghijKLMN'

beforeEach(() => {
  navigate('/setup', true)
  setDismissed(false)
  // the manual route form lists the guests when its step opens
  stubApi({ 'GET /api/v1/guests': { status: 200, body: [] } })
})

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

// A daemon that ran at 12:00:00 with one credential, as nothing else yet.
const ran: State = { ...fresh, at: '2026-10-01T12:00:00Z', finishedAt: '2026-10-01T12:00:02Z', problems: [], complete: true }
const withToken = (patch: Partial<State> = {}, c: CredentialView = good): State => ({ ...ran, credentials: [c], ...patch })
const shallow: CredentialView = { ...good, id: 'c9d0e1f2', label: 'edge', report: good.report && { ...good.report, deep: false } }

async function open(state: State, ui = <SetupPage />, session: Parameters<typeof fakeStore>[0]['session'] = {}) {
  let current = state
  const fake = await fakeStore({
    state,
    session,
    answers: { 'GET /api/v1/state': () => ({ status: 200, body: current, etag: current.digest }) },
  })
  let round = 0
  const deliver = async (next: State) => {
    current = { ...next, digest: `d${++round}` }
    await act(async () => {
      fake.store.notice({ kind: 'state', data: { at: next.at ?? '', finishedAt: next.finishedAt ?? '', digest: current.digest ?? '' } })
      await flush()
    })
  }
  render(
    <StoreProvider store={fake.store}>
      <ToastProvider>{ui}</ToastProvider>
    </StoreProvider>,
  )
  return { store: fake.store, deliver }
}

// What the app does: the view follows the address.
function Routed() {
  return <Page view={useView()} />
}

const step = (name: RegExp | string) => screen.getByRole('button', { name })
const opened = () => screen.getAllByRole('button', { expanded: true }).map((b) => b.textContent)
const region = (name: RegExp | string) => screen.getByRole('region', { name })

describe('the steps', () => {
  test('a fresh install opens the token step, and lists the others closed', async () => {
    await open(fresh)
    expect(screen.getByRole('heading', { level: 1, name: 'First-run setup' })).toBeTruthy()
    expect(screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)).toEqual([
      '1API tokento do',
      '2Zonesnot yet',
      '3Reaching guestsoptional',
      '4First routeto do',
      '5Start publishingnot yet',
    ])
    expect(opened()).toEqual(['1API tokento do'])
  })

  test('a step can be opened and closed by the admin, and stays as chosen', async () => {
    const { deliver } = await open(fresh)
    fireEvent.click(step(/^Reaching guests/))
    expect(opened()).toEqual(['3Reaching guestsoptional'])
    await deliver({ ...fresh, problems: [] })
    expect(opened()).toEqual(['3Reaching guestsoptional'])
    fireEvent.click(step(/^Reaching guests/))
    expect(screen.queryAllByRole('button', { expanded: true })).toHaveLength(0)
  })

  test('the install check is step 0 and comes first while the node is not set up', async () => {
    await open({ ...fresh, problems: ['pco is not set up on this node; run pco setup'] })
    expect(screen.getAllByRole('heading', { level: 2 })[0]?.textContent).toBe('0Install checkneeds attention')
    expect(opened()).toEqual(['0Install checkneeds attention'])
    expect(within(region(/^Install check/)).getByText('pco setup').tagName).toBe('CODE')
    expect(screen.getByText(/Run it as root on the node\./)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Copy' })).toBeTruthy()
    expect(screen.getByText('pco is not set up on this node; run pco setup')).toBeTruthy()
  })

  test('an unknown writer asks for the install check as well, and the check goes when the daemon is set up', async () => {
    const { deliver } = await open({ ...fresh, writerVerdict: 'unknown' })
    expect(opened()).toEqual(['0Install checkneeds attention'])
    await deliver({ ...fresh, writerVerdict: 'ok' })
    expect(screen.queryByText(/^0Install check/)).toBeNull()
    expect(opened()).toEqual(['1API tokento do'])
  })
})

describe('step 1, the token', () => {
  test('says what the token needs and opens both forms of Cloudflare with the name of the node', async () => {
    const node = 'pve 1&x=é/#'
    await open(fresh, <SetupPage />, { node })
    const rows = within(region(/^API token/)).getAllByRole('listitem')
    expect(rows.map((li) => li.querySelector('b')?.textContent)).toEqual(['Account > Cloudflare Tunnel > Edit', 'Zone > DNS > Edit', 'Zone > Zone > Read'])
    const user = screen.getByRole('link', { name: 'Create a user token' })
    const account = screen.getByRole('link', { name: 'Create an account token' })
    expect(user.getAttribute('href')).toBe(userTokenUrl(node))
    expect(account.getAttribute('href')).toBe(accountTokenUrl(node))
    expect(user.getAttribute('href')).toContain('name=pco%20on%20pve%201%26x%3D%C3%A9%2F%23')
    for (const a of [user, account]) {
      expect(a.getAttribute('target')).toBe('_blank')
      expect(a.getAttribute('rel')).toBe('noopener noreferrer')
    }
  })

  async function add(answers: Parameters<typeof stubApi>[0]) {
    const sent = stubApi(answers)
    const shown = await open(fresh)
    fireEvent.change(screen.getByLabelText('Label'), { target: { value: 'edge' } })
    fireEvent.change(screen.getByLabelText('Cloudflare API token'), { target: { value: token } })
    fireEvent.click(screen.getByRole('button', { name: 'Check and add' }))
    return { sent, ...shown }
  }

  test('a token that cannot be used is not stored: the checklist says what to grant, and the step stays open', async () => {
    const refused = {
      label: 'edge',
      kind: 'scoped',
      checked: true,
      id: '',
      report: { ...good.report, usable: false, deep: false },
    }
    const { sent } = await add({
      'POST /api/v1/credentials': {
        status: 400,
        body: { error: 'the token cannot be used: dns.read on example.com: grant Zone > DNS > Read on example.com', code: 'invalid', credential: refused },
      },
    })
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain('the token cannot be used')
    expect(screen.getByText('grant Zone > DNS > Read on example.com')).toBeTruthy()
    expect(screen.getByText(/Usable:/).parentElement?.textContent).toBe('Usable: no')
    expect(screen.queryByRole('button', { name: 'Continue to the zones' })).toBeNull()
    expect((screen.getByLabelText('Cloudflare API token') as HTMLInputElement).value).toBe('')
    expect(opened()).toEqual(['1API tokento do'])
    expect(writes(sent)).toHaveLength(1)
  })

  test('a token that was stored says that write access was not tried, and asks before it tries', async () => {
    const deep: CredentialView = { ...shallow, report: shallow.report && { ...shallow.report, deep: true } }
    const { sent } = await add({
      'POST /api/v1/credentials': { status: 201, body: shallow },
      'POST /api/v1/credentials/c9d0e1f2/check': { status: 200, body: deep },
    })
    await screen.findByText('Write access was not tried.')
    expect(screen.getByRole('button', { name: 'Continue to the zones' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Check write access' }))
    const dialog = document.querySelector<HTMLElement>('dialog[open]')
    if (!dialog) throw new Error('no dialog')
    expect(dialog.textContent).toContain('A deep check creates and deletes a test DNS record and a test tunnel. Continue?')
    expect(writes(sent)).toHaveLength(1)
    fireEvent.click(within(dialog).getByRole('button', { name: 'Check write access' }))
    await waitFor(() => expect(screen.queryByText('Write access was not tried.')).toBeNull())
    expect(writes(sent).at(-1)).toEqual({ method: 'POST', path: '/api/v1/credentials/c9d0e1f2/check', body: { deep: true } })
  })

  test('once the state holds the token that was added, the step shows one checklist of it, and moves on when asked', async () => {
    const { deliver } = await add({ 'POST /api/v1/credentials': { status: 201, body: shallow } })
    await screen.findByText('Write access was not tried.')
    await deliver(withToken({}, shallow))
    expect(screen.getAllByText(/Usable:/)).toHaveLength(1)
    expect(opened()).toEqual(['1API tokendone'])
    fireEvent.click(screen.getByRole('button', { name: 'Continue to the zones' }))
    expect(opened()).toHaveLength(1)
    expect(opened()[0]).toMatch(/^2Zones/)
  })

  test('a stored token that cannot be used shows its checklist open, and is checked again after the grant', async () => {
    const bad: CredentialView = { ...good, report: good.report && { ...good.report, usable: false, deep: false } }
    const checked: CredentialView = { ...bad, report: bad.report && { ...bad.report, usable: true } }
    const sent = stubApi({ 'POST /api/v1/credentials/cred1/check': { status: 200, body: checked } })
    await open(withToken({}, bad))
    const stored = screen.getByRole('region', { name: 'Credential main' })
    expect(within(stored).getByText('problem')).toBeTruthy()
    expect(within(stored).getByText('grant Zone > DNS > Read on example.com')).toBeTruthy()
    expect(within(stored).queryByText('Write access was not tried.')).toBeNull()
    fireEvent.click(within(stored).getByRole('button', { name: 'Check again' }))
    await waitFor(() => expect(within(stored).getByText('usable')).toBeTruthy())
    expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/credentials/cred1/check', body: { deep: false } }])
  })

  test('a stored token that can be used offers the deep check when it never wrote', async () => {
    await open(withToken({}, shallow))
    fireEvent.click(step(/^API token/))
    const stored = screen.getByRole('region', { name: 'Credential edge' })
    expect(within(stored).getByText('Write access was not tried.')).toBeTruthy()
    expect(within(stored).getByRole('button', { name: 'Check write access' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Add another token' })).toBeTruthy()
    expect(screen.queryByRole('form', { name: 'Add a Cloudflare API token' })).toBeNull()
  })
})

describe('step 2, the zones', () => {
  const waiting = withToken({ mode: 'observe' }, { ...good, report: good.report && { ...good.report, checkedAt: '2026-10-01T12:00:05Z' } })

  test('wait for the first cycle after the token was added, then list the zones and what the token leaves out', async () => {
    const { deliver } = await open(waiting)
    fireEvent.click(step(/^Zones/))
    expect(within(region(/^Zones/)).getByText('Waiting for the first cycle with the token')).toBeTruthy()
    expect(within(region(/^Zones/)).getByText('example.org left out: no DNS read')).toBeTruthy()
    expect(within(region(/^Zones/)).getByText('grant Zone > DNS > Edit on example.org')).toBeTruthy()
    await deliver({ ...waiting, zones: full.zones })
    const table = screen.getByRole('table', { name: 'Zones' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows.map((r) => within(r).getAllByRole('cell')[0]?.textContent)).toEqual(['example.com', 'example.info', 'example.net', 'example.org'])
    expect(within(rows[0] as HTMLElement).getByRole('link', { name: 'example.com' }).getAttribute('href')).toBe('/edge/zones/example.com')
    expect(screen.queryByText('Waiting for the first cycle with the token')).toBeNull()
  })

  test('say so when a cycle after the check found no zone', async () => {
    await open(withToken({}, { ...good, report: good.report && { ...good.report, checkedAt: '2026-10-01T11:00:00Z' } }))
    expect(opened()).toEqual(['2Zonesneeds attention'])
    expect(within(region(/^Zones/)).getByText(/found no zone/)).toBeTruthy()
  })

  test('without a usable token, the step points at the token', async () => {
    await open(fresh)
    fireEvent.click(step(/^Zones/))
    fireEvent.click(within(region(/^Zones/)).getByRole('button', { name: 'Add a token first' }))
    expect(opened()).toEqual(['1API tokento do'])
  })

  test('a zone that two credentials list and nobody pinned needs a pin', async () => {
    const two = { ...(full.zones[0] as ZoneView), pinned: undefined }
    await open(withToken({ zones: [two] }))
    fireEvent.click(step(/^Zones/))
    expect(within(region(/^Zones/)).getByText('needs a pin')).toBeTruthy()
  })
})

describe('step 3, reaching guests', () => {
  test('lists the bridges where routes were proven and sends the choice of the level to the settings', async () => {
    await open(withToken({ routes: full.routes, zones: full.zones }, good), <SetupPage />)
    fireEvent.click(step(/^Reaching guests/))
    const body = region(/^Reaching guests/)
    const [bridge] = within(within(body).getByRole('list')).getAllByRole('listitem')
    expect(bridge?.textContent).toBe('vmbr0 VLAN 20: 1 route')
    expect(within(body).getByRole('link', { name: 'Settings' }).getAttribute('href')).toBe('/settings')
    expect(within(body).getByRole('link', { name: 'Networks' }).getAttribute('href')).toBe('/networks')
  })
})

describe('step 4, the first route', () => {
  const ready = withToken({ mode: 'observe', zones: full.zones, routes: [], gateTagged: 1 })

  async function build() {
    const shown = await open(ready)
    const guest = within(region('Annotate a guest'))
    fireEvent.change(guest.getByLabelText('Name'), { target: { value: 'App' } })
    fireEvent.change(guest.getByLabelText('Port'), { target: { value: '3000' } })
    return { ...shown, guest }
  }

  test('builds the block for the Notes from a name, the zone of step 2 and a port, with a copy button, and the tag to set', async () => {
    const { guest } = await build()
    expect(opened()).toEqual(['4First routeto do'])
    expect(screen.getByText(/Give a guest the tag/).textContent).toContain('cf-tunnel')
    expect(guest.getByLabelText('Zone').querySelectorAll('option')).toHaveLength(1)
    const block = screen.getByLabelText('The block for the Notes')
    expect(block.textContent).toBe('```cf-tunnel\napp.example.com -> :3000\n```')
    const write = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    await act(async () => {
      fireEvent.click(guest.getByRole('button', { name: 'Copy' }))
    })
    expect(write).toHaveBeenCalledWith('```cf-tunnel\napp.example.com -> :3000\n```')
    expect(screen.getByText('Copied.')).toBeTruthy()
  })

  test('the options show in the block; https adds its own', async () => {
    const { guest } = await build()
    fireEvent.change(guest.getByLabelText('Scheme'), { target: { value: 'https' } })
    fireEvent.change(guest.getByLabelText('Port'), { target: { value: '8443' } })
    fireEvent.click(guest.getByLabelText(/no-tls-verify/))
    fireEvent.change(guest.getByLabelText('Via (optional)'), { target: { value: 'net1' } })
    expect(screen.getByLabelText('The block for the Notes').textContent).toBe('```cf-tunnel\napp.example.com -> https://:8443 no-tls-verify via=net1\n```')
  })

  test('no block while a value is wrong: the field says why, and nothing is waited for', async () => {
    const { guest } = await build()
    fireEvent.change(guest.getByLabelText('Port'), { target: { value: '70000' } })
    expect(guest.getByText('want a port from 1 to 65535, without leading zeros')).toBeTruthy()
    expect(screen.queryByLabelText('The block for the Notes')).toBeNull()
    expect(screen.queryByText(/Waiting for a route for/)).toBeNull()
  })

  test('waits for a route of that hostname in the state, and shows it as it moves, with its diagnosis a click away', async () => {
    const { deliver } = await build()
    expect(screen.getByText(/Waiting for a route for/).textContent).toContain('app.example.com')
    const route: RouteView = { ...www, hostname: 'app.example.com', owner: 'qemu/101', state: 'unreachable', reason: 'no verified address yet' }
    await deliver({ ...ready, routes: [route] })
    const status = screen.getByText('no verified address yet').closest('p') as HTMLElement
    expect(within(status).getByText('unreachable')).toBeTruthy()
    expect(within(status).getByRole('link', { name: 'Diagnose it' }).getAttribute('href')).toBe('/routes/app.example.com?owner=qemu%2F101')
    await deliver({ ...ready, routes: [{ ...route, state: 'active', reason: undefined }] })
    expect(within(screen.getByText('Diagnose it').closest('p') as HTMLElement).getByText('active')).toBeTruthy()
    expect(screen.queryByText('no verified address yet')).toBeNull()
  })

  test('a route of another hostname is not the one waited for', async () => {
    const { deliver } = await build()
    await deliver({ ...ready, routes: [{ ...www, hostname: 'other.example.com', owner: 'qemu/102' }] })
    expect(screen.getByText(/Waiting for a route for/)).toBeTruthy()
    expect(screen.getByText('The state holds 1 route:')).toBeTruthy()
  })

  test('without a zone the hostname is typed whole', async () => {
    await open(withToken({ zones: [], routes: [] }))
    const guest = within(region('Annotate a guest'))
    fireEvent.change(guest.getByLabelText('Hostname'), { target: { value: 'wiki.example.com' } })
    fireEvent.change(guest.getByLabelText('Port'), { target: { value: '8080' } })
    expect(screen.getByLabelText('The block for the Notes').textContent).toBe('```cf-tunnel\nwiki.example.com -> :8080\n```')
  })

  test('a manual route is the other way, with the form of the Routes page', async () => {
    await open(ready)
    const manual = within(region('Or make a manual route'))
    expect(manual.getByLabelText('Hostname')).toBeTruthy()
    expect(manual.getByRole('button', { name: 'Make the route' })).toBeTruthy()
  })
})

describe('step 5, start publishing', () => {
  test('is closed without a usable credential and without a route, says which is missing, and links the step that adds it', async () => {
    const sent = stubApi({})
    await open(fresh)
    fireEvent.click(step(/^Start publishing/))
    const body = region(/^Start publishing/)
    expect(within(body).getByText('a Cloudflare API token that can be used:')).toBeTruthy()
    expect(within(body).getByText('a route:')).toBeTruthy()
    expect(within(body).queryByRole('button', { name: 'Start publishing' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Pending actions' })).toBeNull()
    fireEvent.click(within(body).getByRole('button', { name: 'Step 4' }))
    expect(opened()).toEqual(['4First routeto do'])
    fireEvent.click(step(/^Start publishing/))
    fireEvent.click(within(region(/^Start publishing/)).getByRole('button', { name: 'Step 1' }))
    expect(opened()).toEqual(['1API tokento do'])
    expect(writes(sent)).toEqual([])
  })

  test('is closed with a route and no usable token, naming the token only', async () => {
    await open({ ...fresh, routes: full.routes })
    fireEvent.click(step(/^Start publishing/))
    const body = region(/^Start publishing/)
    expect(within(body).getByText('a Cloudflare API token that can be used:')).toBeTruthy()
    expect(within(body).queryByText('a route:')).toBeNull()
  })

  test('is closed with a usable token and no route, naming the route only', async () => {
    await open(withToken({ zones: full.zones, routes: [] }))
    fireEvent.click(step(/^Start publishing/))
    const body = region(/^Start publishing/)
    expect(within(body).getByText('a route:')).toBeTruthy()
    expect(within(body).queryByText('a Cloudflare API token that can be used:')).toBeNull()
  })

  const observing = withToken({ mode: 'observe', zones: full.zones, routes: full.routes, actions: full.actions.filter((a) => !a.applied), waiting: [] })

  test('opens with both, shows the plan and what it will do, and starts publishing without any confirmation field', async () => {
    const sent = stubApi({ 'POST /api/v1/apply': { status: 200, body: { leftObserveOnly: true, accepted: [] } } })
    await open(observing)
    expect(opened()).toEqual(['5Start publishingto do'])
    const body = region(/^Start publishing/)
    expect(within(within(body).getByRole('table', { name: 'Pending actions' })).getByText('delete-record')).toBeTruthy()
    expect(within(body).getByRole('heading', { level: 3, name: 'Pending actions' })).toBeTruthy()
    fireEvent.click(within(body).getByRole('button', { name: 'Start publishing' }))
    await within(body).findByText(applying)
    expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/apply', body: {} }])
    expect(within(body).queryByRole('button', { name: 'Start publishing' })).toBeNull()
    expect(within(body).getByRole('link', { name: 'Follow it on the Overview.' }).getAttribute('href')).toBe('/')
  })

  test('says it when observe-only mode was off already', async () => {
    stubApi({ 'POST /api/v1/apply': { status: 200, body: { leftObserveOnly: false, accepted: [] } } })
    await open(observing)
    fireEvent.click(screen.getByRole('button', { name: 'Start publishing' }))
    await screen.findByText(applyingAlways)
  })

  test('says why when the daemon refuses', async () => {
    stubApi({ 'POST /api/v1/apply': { status: 409, body: { error: 'refused: no credential can be used', code: 'refused' } } })
    await open(observing)
    fireEvent.click(screen.getByRole('button', { name: 'Start publishing' }))
    expect((await screen.findByRole('alert')).textContent).toContain('no credential can be used')
    expect(screen.getByRole('button', { name: 'Start publishing' })).toBeTruthy()
  })

  test('is done once the daemon applies', async () => {
    await open({ ...observing, mode: 'enforce' })
    expect(screen.getByText(/Publishing has started: the daemon changes Cloudflare in every cycle\./)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Start publishing' })).toBeNull()
  })
})

describe('the first page', () => {
  test('is the setup, at its own address, while there is no credential', async () => {
    navigate('/', true)
    await open(fresh, <Start />)
    expect(window.location.pathname).toBe('/setup')
    expect(screen.getByRole('heading', { level: 1, name: 'First-run setup' })).toBeTruthy()
  })

  test('is the Overview once there is a credential', async () => {
    navigate('/', true)
    await open(full, <Start />)
    expect(window.location.pathname).toBe('/')
    expect(screen.getByRole('heading', { level: 1, name: 'Overview' })).toBeTruthy()
  })

  test('stays the setup while the steps are done, and does not throw the admin out when the credential arrives', async () => {
    navigate('/', true)
    const { deliver } = await open(fresh, <Routed />)
    expect(window.location.pathname).toBe('/setup')
    await deliver(withToken({ zones: full.zones }))
    expect(window.location.pathname).toBe('/setup')
    expect(screen.getByRole('heading', { level: 1, name: 'First-run setup' })).toBeTruthy()
  })

  test('skipping is kept in this browser, and the Overview comes first from then on', async () => {
    navigate('/setup', true)
    await open(fresh, <Page view={match('/setup')} />)
    expect(window.localStorage.getItem(dismissedKey)).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Skip for now' }))
    expect(window.localStorage.getItem(dismissedKey)).not.toBeNull()
    expect(window.location.pathname).toBe('/')
  })

  test('the Overview is shown while the setup is skipped, and the setup says so and can be asked for again', async () => {
    setDismissed(true)
    navigate('/', true)
    await open(fresh, <Start />)
    expect(window.location.pathname).toBe('/')
    expect(screen.getByRole('heading', { level: 1, name: 'Overview' })).toBeTruthy()
    cleanup()
    navigate('/setup', true)
    await open(fresh)
    expect(screen.getByText(/You skipped the setup in this browser/).textContent).toContain('kept per browser')
    expect(screen.queryByRole('button', { name: 'Skip for now' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Show the setup first again' }))
    expect(window.localStorage.getItem(dismissedKey)).toBeNull()
    expect(screen.queryByText(/You skipped the setup/)).toBeNull()
  })

  test('the setup is reachable from the Settings page', async () => {
    stubApi({ 'GET /api/v1/settings': { status: 200, body: {} } })
    navigate('/settings', true)
    await open(full, <Page view={match('/settings')} />)
    expect(screen.getByRole('link', { name: 'First-run setup' }).getAttribute('href')).toBe('/setup')
  })
})

describe('a reader', () => {
  test('is told that an admin has to finish the setup, sees how far it is, and cannot take a step', async () => {
    await open(fresh, <SetupPage />, { role: 'reader' })
    expect(screen.getByText('An admin has to finish the setup')).toBeTruthy()
    expect(screen.queryByRole('button', { name: /^1API token/ })).toBeNull()
    expect(screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)).toContain('1API tokento do')
    expect(screen.queryByLabelText('Cloudflare API token')).toBeNull()
  })
})
