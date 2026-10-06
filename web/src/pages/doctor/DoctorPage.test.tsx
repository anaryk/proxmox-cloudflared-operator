import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../../api/store'
import type { Finding } from '../../api/types.gen'
import doctor from '../../fixtures/doctor.json'
import counts from '../../fixtures/doctor-counts.json'
import untagged from '../../fixtures/untagged.json'
import { fakeStore } from '../../test/store'
import { DoctorPage } from './DoctorPage'

afterEach(() => {
  vi.unstubAllGlobals()
})

const answer = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })

function stub(...answers: Response[]) {
  const fetch = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async () => answers.shift() ?? answer([]))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

async function mount(role = 'admin') {
  const fake = await fakeStore({ state: untagged, session: { role } })
  render(
    <StoreProvider store={fake.store}>
      <DoctorPage />
    </StoreProvider>,
  )
  return fake
}

const run = (name = 'Run the checks') => fireEvent.click(screen.getByRole('button', { name }))
const section = (name: string) => screen.getByRole('region', { name })
const copyButtons = () => screen.queryAllByRole('button', { name: 'Copy' })

test('runs nothing until it is asked to', async () => {
  const fetch = stub()
  await mount()
  expect(fetch).not.toHaveBeenCalled()
  expect(screen.getByText('Not run yet')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Run the checks' })).toBeTruthy()
})

describe('an admin', () => {
  test('gets the findings by their level, and the failures of the run for the navigation', async () => {
    const fetch = stub(answer(doctor))
    const { store } = await mount()
    run()
    expect(await screen.findByRole('heading', { name: 'Failures' })).toBeTruthy()
    expect(fetch).toHaveBeenCalledTimes(1)
    const [url, init] = fetch.mock.calls[0] ?? []
    expect(url).toBe('/api/v1/doctor')
    expect(init?.method).toBe('POST')
    expect(init?.body).toBe('{}')

    expect(screen.getByText(/^5 ok, 5 warnings, 3 failures, run at$/)).toBeTruthy()
    const failed = within(section('Failures'))
    expect(failed.getAllByRole('listitem')).toHaveLength(3)
    expect(failed.getByText('egress')).toBeTruthy()
    expect(failed.getByText('the egress filter is switched off since 2026-10-01T11:00:00Z: the connectors are not confined')).toBeTruthy()
    expect(within(section('Warnings')).getAllByRole('listitem')).toHaveLength(5)
    expect(within(section('Passed')).getAllByRole('listitem')).toHaveLength(5)

    expect(store.get().doctorLast?.fail).toBe(3)
    expect(screen.getByRole('button', { name: 'Run again' })).toBeTruthy()
  })

  test('gets a button to copy a fix that is a command, and the words of any other', async () => {
    stub(answer(doctor))
    await mount()
    run()
    await screen.findByRole('heading', { name: 'Failures' })
    expect(screen.getAllByRole('code').map((c) => c.textContent)).toEqual([
      'pco status',
      'pco egress on',
      'pco guest approve lxc/201',
      'pco guest approve lxc/202',
      'pco apply --confirm-deletes',
    ])
    expect(copyButtons()).toHaveLength(5)
    // a credential's id has no form a command may carry, and a name is a placeholder
    for (const text of ['pco credential check cred1', 'pco adopt <name>', 'journalctl -u pco-cloudflared@pco-abc123']) {
      expect(screen.getByText(text).closest('code')).toBeNull()
    }
  })

  test('gets a command only when the fix is nothing but one', async () => {
    const quoting = (fix: string): Finding => ({ check: 'approval qemu/1', level: 'warn', detail: 'a guest waits', fix })
    stub(
      answer([
        quoting('pco guest approve qemu/101'),
        quoting('journalctl -u pco says why it does not come'),
        quoting('pco guest approve qemu/1; reboot'),
        quoting('pco guest approve qemu/1 && reboot'),
        quoting('pco guest approve qemu/1‮'),
      ]),
    )
    await mount()
    run()
    await screen.findByRole('heading', { name: 'Warnings' })
    expect(copyButtons()).toHaveLength(1)
    expect(screen.getAllByRole('code').map((c) => c.textContent)).toEqual(['pco guest approve qemu/101'])
    expect(screen.getByText('journalctl -u pco says why it does not come')).toBeTruthy()
    expect(screen.getByText('pco guest approve qemu/1; reboot')).toBeTruthy()
    expect(screen.getByText('pco guest approve qemu/1 && reboot')).toBeTruthy()
    // what is not plain text is shown as what it is
    expect(screen.getByText('pco guest approve qemu/1')).toBeTruthy()
    expect(screen.getByText('⟨U+202E⟩')).toBeTruthy()
  })

  test('says what a level it does not know is, as the daemon wrote it', async () => {
    stub(answer([{ check: 'newer', level: 'note', detail: 'a level of a newer daemon' }]))
    await mount()
    run()
    expect(await screen.findByRole('heading', { name: 'Other' })).toBeTruthy()
    expect(within(section('Other')).getByText('note')).toBeTruthy()
  })

  test('runs again when asked, and not by itself', async () => {
    const fetch = stub(answer(doctor), answer([{ check: 'mode', level: 'ok', detail: 'enforce: changes are applied' }]))
    const { store } = await mount()
    run()
    await screen.findByRole('heading', { name: 'Failures' })
    run('Run again')
    await screen.findByText(/^1 ok, 0 warnings, 0 failures, run at$/)
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(screen.queryByRole('heading', { name: 'Failures' })).toBeNull()
    expect(store.get().doctorLast?.fail).toBe(0)
  })

  test('is told how long it runs, and has no second run under way', async () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(() => {})))
    await mount()
    run()
    expect(screen.getByText('Running the checks')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Run the checks' })).toBeNull()
  })

  test('is told why the run did not happen, and may try again', async () => {
    stub(answer({ code: 'rate_limited', error: 'doctor runs: 2 per minute', retryAfter: 30 }, 429), answer(doctor))
    await mount()
    run()
    expect((await screen.findByText(/The doctor did not run:/)).textContent).toContain('Try again in 30 s.')
    expect(screen.getByText('Not run yet')).toBeTruthy()
    run()
    expect(await screen.findByRole('heading', { name: 'Failures' })).toBeTruthy()
    expect(screen.queryByText(/The doctor did not run:/)).toBeNull()
  })

  test('is shown the failures of the last run in this browser before it runs', async () => {
    const { store } = await mount()
    store.setDoctorLast({ fail: 1, at: '2026-10-01T12:01:05Z' })
    expect(await screen.findByText(/The last run in this browser had 1 failure, at/)).toBeTruthy()
  })
})

describe('a reader', () => {
  test('gets the counts and is told the findings are for admins, before and after', async () => {
    const fetch = stub(answer(counts))
    const { store } = await mount('reader')
    expect(screen.getByText(/The findings are for admins/)).toBeTruthy()
    run()
    expect(await screen.findByText(/^12 ok, 2 warnings, 1 failure, run at$/)).toBeTruthy()
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(screen.getByText(/The findings are for admins: they name guests waiting for approval and hostnames of conflicts and lost markers/)).toBeTruthy()
    expect(screen.queryByRole('heading', { name: 'Failures' })).toBeNull()
    // the counter in the navigation is the admins'
    expect(store.get().doctorLast).toBeUndefined()
  })

  test('gets no command, whatever the daemon answered', async () => {
    stub(answer(counts))
    await mount('reader')
    run()
    await screen.findByText(/^12 ok/)
    expect(copyButtons()).toHaveLength(0)
  })
})

test('an answer it does not know is an error, not an empty list', async () => {
  stub(answer({ unexpected: true }))
  await mount()
  run()
  expect(await screen.findByText(/The doctor did not run:/)).toBeTruthy()
})
