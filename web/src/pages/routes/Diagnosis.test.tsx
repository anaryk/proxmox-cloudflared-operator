import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { type AppStore, StoreProvider } from '../../api/store'
import type { State, Step } from '../../api/types.gen'
import { ToastProvider } from '../../components/Toast'
import golden from '../../fixtures/diagnose.json'
import populated from '../../fixtures/populated.json'
import { fakeStore, flush } from '../../test/store'
import { Diagnosis, skippedWord } from './Diagnosis.tsx'
import { diagnoses } from './diagnosis.ts'
import { RouteDetail } from './RouteDetail'

// The golden of the daemon with the steps before identity passing, identity
// failing, and the two after it skipped.
const steps: Step[] = (golden as Step[]).map((s, i) => {
  if (i < 5) return { name: s.name, level: 'ok', detail: `${s.name} is as it should be` }
  if (i === 5) return { name: s.name, level: 'fail', detail: 'the guest answered with another identity' }
  return s
})

function show(store: AppStore, hostname = 'www.example.com') {
  return render(
    <StoreProvider store={store}>
      <ToastProvider>
        <Diagnosis hostname={hostname} />
      </ToastProvider>
    </StoreProvider>,
  )
}

type Answer = () => Response | Promise<Response>

function stubFetch(answer: Answer) {
  const fetch = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async () => answer())
  vi.stubGlobal('fetch', fetch)
  return fetch
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })

afterEach(() => {
  vi.unstubAllGlobals()
  diagnoses.clear()
})

describe('a diagnosis', () => {
  test('is a POST with the hostname in the body', async () => {
    const fetch = stubFetch(() => json(steps))
    const { store } = await fakeStore({ state: populated })
    show(store)
    fireEvent.click(screen.getByRole('button', { name: 'Run diagnosis' }))
    await screen.findByRole('list', { name: 'Steps of the diagnosis' })
    expect(fetch).toHaveBeenCalledTimes(1)
    const [url, init] = fetch.mock.calls[0] ?? []
    expect(url).toBe('/api/v1/diagnose')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({ hostname: 'www.example.com' })
  })

  test('shows every step at once, the skipped ones greyed and the first failure open', async () => {
    stubFetch(() => json(steps))
    const { store } = await fakeStore({ state: populated })
    show(store)
    fireEvent.click(screen.getByRole('button', { name: 'Run diagnosis' }))
    const list = await screen.findByRole('list', { name: 'Steps of the diagnosis' })
    // in the render that shows the first step, all eight are there
    const items = within(list).getAllByRole('listitem')
    expect(items.map((li) => li.querySelector('.step-name')?.textContent)).toEqual(['route', 'zone', 'dns', 'ingress', 'connector', 'identity', 'tcp', 'http'])
    const skipped = items.filter((li) => li.classList.contains('step-skipped'))
    expect(skipped.map((li) => li.querySelector('.step-name')?.textContent)).toEqual(['tcp', 'http'])
    for (const li of skipped) {
      expect(li.querySelector('.status-word')?.textContent).toBe(skippedWord)
      expect(li.querySelector('.step-detail')).toBeNull()
    }
    const failed = items[5] as HTMLElement
    expect(failed.classList.contains('step-fail')).toBe(true)
    // open, not behind a "Details"
    expect(failed.querySelector(':scope > .step-detail')?.textContent).toBe('the guest answered with another identity')
    expect(items[0]?.querySelector('details .step-detail')?.textContent).toBe('route is as it should be')
    expect(screen.getByText(/Run in this browser at/)).toBeTruthy()
  })

  test('while it runs: the time it has run, and the button refused', async () => {
    let answer: (r: Response) => void = () => {}
    stubFetch(() => new Promise<Response>((r) => (answer = r)))
    const { store } = await fakeStore({ state: populated })
    show(store)
    fireEvent.click(screen.getByRole('button', { name: 'Run diagnosis' }))
    expect(screen.getByRole('status').textContent).toBe('Diagnosing')
    expect(screen.getByText('0 s')).toBeTruthy()
    const button = screen.getByRole('button', { name: 'Run diagnosis' })
    expect(button.getAttribute('aria-disabled')).toBe('true')
    fireEvent.click(button)
    await act(async () => {
      answer(json(steps))
      await flush()
    })
    expect(screen.getByRole('button', { name: 'Run diagnosis' }).getAttribute('aria-disabled')).toBeNull()
  })

  test('too many: the time to wait', async () => {
    stubFetch(() => json({ error: 'too many diagnoses', code: 'rate_limited', retryAfter: 12 }, 429))
    const { store } = await fakeStore({ state: populated })
    show(store)
    fireEvent.click(screen.getByRole('button', { name: 'Run diagnosis' }))
    expect((await screen.findByRole('alert')).textContent).toBe('Try again in 12 s.')
  })

  test('another holder by the time it ran: look at the route again', async () => {
    stubFetch(() => json({ error: 'the holder changed: www.example.com is no longer held by qemu/101', code: 'holder_changed' }, 409))
    const { store } = await fakeStore({ state: populated })
    show(store)
    fireEvent.click(screen.getByRole('button', { name: 'Run diagnosis' }))
    expect((await screen.findByRole('alert')).textContent).toBe('Another guest holds this hostname now. Look at the route again before you diagnose it.')
  })

  test('a result kept in this browser is marked once the state changed since', async () => {
    stubFetch(() => json(steps))
    let current = populated as unknown as State
    const { store } = await fakeStore({
      state: populated,
      answers: { 'GET /api/v1/state': () => ({ status: 200, body: current, etag: current.digest }) },
    })
    const { unmount } = show(store)
    fireEvent.click(screen.getByRole('button', { name: 'Run diagnosis' }))
    await screen.findByRole('list', { name: 'Steps of the diagnosis' })
    expect(screen.queryByText('the state changed since')).toBeNull()
    unmount()
    // the result outlives the panel that showed it
    show(store)
    expect(screen.getByRole('list', { name: 'Steps of the diagnosis' })).toBeTruthy()
    current = { ...current, digest: '77a1000000000000' }
    await act(async () => {
      store.notice({ kind: 'state', data: { at: '2026-10-01T12:00:10Z', finishedAt: '2026-10-01T12:00:12Z', digest: '77a1000000000000' } })
      await flush()
    })
    expect(screen.getByText('the state changed since')).toBeTruthy()
    expect(screen.getByRole('list', { name: 'Steps of the diagnosis' })).toBeTruthy()
  })
})

describe('the tab of the diagnosis', () => {
  const tabs = () => screen.getAllByRole('tab').map((t) => t.textContent)

  beforeEach(() => {
    stubFetch(() => json({ error: 'no target', code: 'not_found' }, 404))
  })

  test('names the holder, and runs for the hostname', async () => {
    const { store } = await fakeStore({ state: populated })
    render(
      <StoreProvider store={store}>
        <ToastProvider>
          <RouteDetail hostname="www.example.com" owner="qemu/101" variant="drawer" />
        </ToastProvider>
      </StoreProvider>,
    )
    expect(tabs()).toEqual(['Overview', 'Diagnosis of the current holder', 'Timeline', 'Claim'])
  })

  test('is not there for a route that lost its hostname, which links the holder instead', async () => {
    const { store } = await fakeStore({ state: populated })
    render(
      <StoreProvider store={store}>
        <ToastProvider>
          <RouteDetail hostname="www.example.com" owner="qemu/102" variant="drawer" />
        </ToastProvider>
      </StoreProvider>,
    )
    expect(tabs()).toEqual(['Overview', 'Timeline', 'Claim'])
    expect(screen.getByRole('link', { name: 'the route of qemu/101' }).getAttribute('href')).toBe('/routes/www.example.com?owner=qemu%2F101')
  })
})
