import { fireEvent, render, screen, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { type AppStore, StoreProvider } from '../../api/store'
import type { State, Waiting } from '../../api/types.gen'
import { navigate } from '../../app/router'
import { ToastProvider } from '../../components/Toast'
import populated from '../../fixtures/populated.json'
import { fakeStore } from '../../test/store'
import { budgetLine, counts, PlanPage, PlanSections, waitingId } from './PlanPage'

const golden = populated as unknown as State
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })

function show(store: AppStore, ui: ReactNode) {
  return render(
    <StoreProvider store={store}>
      <ToastProvider>{ui}</ToastProvider>
    </StoreProvider>,
  )
}

function stubFetch(handle: (url: string, init?: RequestInit) => Response) {
  const fetch = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async (url, init) => handle(url, init))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

beforeEach(() => {
  navigate('/routes/plan', true)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the plan', () => {
  test('the four sections of pco plan, in its words', async () => {
    const { store } = await fakeStore({ state: populated })
    show(store, <PlanSections />)
    expect(screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)).toEqual([
      'Pending actions',
      'Waits for a confirmation',
      'Records of someone else that stand in the way',
      'Names that point at the tunnel but lost the marker of this install',
    ])
    // only what was not applied, the destructive one marked
    const table = screen.getByRole('table', { name: 'Pending actions' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(1)
    expect(rows[0]?.textContent).toBe('delete-record destructiveold.example.comin zone example.comgrace period: 1m0s left')
    expect(rows[0]?.className).toContain('row-destructive')
    expect(screen.getByText('Destructive actions that are pending; a confirmation does not affect them:')).toBeTruthy()
    expect(screen.getByText('192.0.2.10')).toBeTruthy()
    expect(screen.getByText('lost.example.com')).toBeTruthy()
    expect(screen.queryByText('Nothing to do.')).toBeNull()
  })

  test('nothing to do, as pco plan says it', async () => {
    const { store } = await fakeStore({ state: { ...golden, actions: [], waiting: [], conflicts: [], lost: [], offer: undefined } })
    show(store, <PlanSections />)
    expect(screen.getByText('Nothing to do.')).toBeTruthy()
  })

  test('the compact plan of the wizard has the same sections, as headings of a step', async () => {
    const { store } = await fakeStore({ state: populated })
    show(store, <PlanSections compact />)
    expect(screen.getAllByRole('heading', { level: 3 })).toHaveLength(4)
    expect(screen.queryAllByRole('heading', { level: 2 })).toHaveLength(0)
  })

  test('the first 20 items of an entry, then the rest on request', async () => {
    const many: Waiting = { ...(golden.waiting[0] as Waiting), items: Array.from({ length: 23 }, (_, i) => `r${String(i).padStart(2, '0')}.example.com`) }
    const { store } = await fakeStore({ state: { ...golden, waiting: [many] } })
    show(store, <PlanSections />)
    expect(screen.queryByText('r19.example.com')).toBeTruthy()
    expect(screen.queryByText('r20.example.com')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'and 3 more' }))
    expect(screen.getByText('r22.example.com')).toBeTruthy()
  })

  test('an entry a link points at is marked', async () => {
    const zone = golden.waiting[1] as Waiting
    expect(waitingId(zone)).toBe('waiting-stale-zone-example.info')
    navigate('/routes/plan#waiting-stale-zone-example.info', true)
    const { store } = await fakeStore({ state: populated })
    show(store, <PlanSections />)
    const entry = document.getElementById('waiting-stale-zone-example.info')
    expect(entry?.classList.contains('highlighted')).toBe(true)
    expect(entry?.textContent).toBe(zone.detail)
  })

  test('Review and confirm opens the confirmation; a reader is told why not', async () => {
    const admin = await fakeStore({ state: populated })
    const { unmount } = show(admin.store, <PlanSections />)
    fireEvent.click(screen.getByRole('button', { name: 'Review and confirm' }))
    expect(within(screen.getByRole('dialog')).getByText('Confirm what waits')).toBeTruthy()
    unmount()
    const reader = await fakeStore({ state: populated, session: { role: 'reader' } })
    show(reader.store, <PlanSections />)
    fireEvent.click(screen.getByRole('button', { name: 'Review and confirm' }))
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})

describe("the budget's note", () => {
  const line = '1 change waits for Cloudflare\'s rate limit'

  test('the problem line of the budget stop, above the sections', async () => {
    const { store } = await fakeStore({ state: { ...golden, problems: ['a problem', line] } })
    show(store, <PlanSections />)
    const note = screen.getByText(line).closest('.banner') as HTMLElement
    expect(note.classList.contains('banner-warn')).toBe(true)
    expect(note.textContent).toBe(`${line}: the pending actions wait for it, and the next cycles carry them out as the rate limit allows.`)
    expect(note.compareDocumentPosition(screen.getByRole('heading', { name: 'Pending actions' })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  test('no note for a line that only ends in the same words', async () => {
    expect(budgetLine(['the listing of zone example.com and 3 changes wait for Cloudflare\'s rate limit'])).toBeDefined()
    expect(budgetLine(['someone wrote: 3 changes wait for Cloudflare\'s rate limit'])).toBeUndefined()
    const { store } = await fakeStore({ state: populated })
    show(store, <PlanSections />)
    expect(document.querySelector('.plan .banner')).toBeNull()
  })
})

describe('the actions of the plan', () => {
  test('Adopt asks first, then sends the name', async () => {
    const fetch = stubFetch((url) => (url === '/api/v1/adopt' ? json({}) : json({ error: 'none', code: 'not_found' }, 404)))
    const { store } = await fakeStore({ state: populated })
    show(store, <PlanSections />)
    fireEvent.click(screen.getByRole('button', { name: 'Adopt' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText(/is held by a record of someone else/).textContent).toBe(
      'api.example.com is held by a record of someone else in zone example.com: A 192.0.2.10. Adopting replaces it with a record that points at the tunnel.',
    )
    expect(within(dialog).getByText('/etc/pve/pco/adopted.jsonl')).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Adopt it' }))
    await screen.findByText(/Adoption of/)
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(fetch.mock.calls[0]?.[0]).toBe('/api/v1/adopt')
    expect(JSON.parse(String(fetch.mock.calls[0]?.[1]?.body))).toEqual({ name: 'api.example.com' })
  })

  test('Take back says the name lost its marker', async () => {
    stubFetch(() => json({}))
    const { store } = await fakeStore({ state: populated })
    show(store, <PlanSections />)
    fireEvent.click(screen.getByRole('button', { name: 'Take back' }))
    expect(within(screen.getByRole('dialog')).getByText(/lost its marker/).textContent).toBe(
      'lost.example.com points at the tunnel of this install but lost its marker. Adopting takes the record back.',
    )
  })

  test('Start publishing, in observe-only mode, after the counts of the plan', async () => {
    const fetch = stubFetch((url) => (url === '/api/v1/apply' ? json({ accepted: [], leftObserveOnly: true }) : json({}, 404)))
    const { store } = await fakeStore({ state: { ...golden, mode: 'observe' } })
    show(store, <PlanPage />)
    fireEvent.click(screen.getByRole('button', { name: 'Start publishing' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText('delete-record').closest('li')?.textContent).toBe('1 delete-record')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Start publishing' }))
    await screen.findByText('Publishing has started: the daemon changes Cloudflare from the next cycle.')
    expect(JSON.parse(String(fetch.mock.calls[0]?.[1]?.body))).toEqual({ confirmDeletes: false, offer: '' })
  })

  test('no Start publishing while pco publishes', async () => {
    const { store } = await fakeStore({ state: populated })
    show(store, <PlanPage />)
    expect(screen.queryByRole('button', { name: 'Start publishing' })).toBeNull()
  })
})

test('counts of the pending actions by kind', () => {
  expect(counts(golden.actions)).toEqual([['delete-record', 1]])
})
