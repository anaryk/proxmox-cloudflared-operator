import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { type AppStore, StoreProvider } from '../../api/store'
import type { State, Waiting } from '../../api/types.gen'
import { ToastProvider } from '../../components/Toast'
import populated from '../../fixtures/populated.json'
import { fakeStore, flush } from '../../test/store'
import { ConfirmDialog } from './ConfirmDialog'

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })

const golden = populated as unknown as State
const [removals, zone, tunnel, vanished] = golden.waiting as [Waiting, Waiting, Waiting, Waiting]
const newOffer = '1234567890abcdef'
const changed: State = {
  ...golden,
  digest: '77a1000000000000',
  offer: newOffer,
  waiting: [{ ...removals, items: [...removals.items, 'c.example.com'] }, zone, vanished],
}

interface Sent {
  confirmDeletes: boolean
  offer: string
}

// stubApply answers POST /api/v1/apply as the daemon would: what was offered
// by the state now is accepted, any other offer refused.
function stubApply(offered: () => State, leftObserveOnly = false) {
  const sent: Sent[] = []
  const fetch = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async (url, init) => {
    if (url !== '/api/v1/apply' || init?.method !== 'POST') return json({ error: 'none', code: 'not_found' }, 404)
    const body = JSON.parse(String(init.body)) as Sent
    sent.push(body)
    const now = offered()
    if (body.offer !== now.offer) {
      return json({ error: 'refused: what waits for a confirmation changed since it was shown; look again and repeat', code: 'refused' }, 409)
    }
    return json({ accepted: now.waiting, leftObserveOnly })
  })
  vi.stubGlobal('fetch', fetch)
  return sent
}

async function stream(st: State) {
  let current = st
  const fake = await fakeStore({
    state: st,
    answers: { 'GET /api/v1/state': () => ({ status: 200, body: current, etag: current.digest }) },
  })
  const deliver = async (next: State) => {
    current = next
    await act(async () => {
      fake.store.notice({ kind: 'state', data: { at: '2026-10-01T12:00:10Z', finishedAt: '2026-10-01T12:00:12Z', digest: next.digest ?? '' } })
      await flush()
    })
  }
  return { store: fake.store, deliver, now: () => current }
}

function show(store: AppStore, onClose = () => {}) {
  render(
    <StoreProvider store={store}>
      <ToastProvider>
        <ConfirmDialog onClose={onClose} />
      </ToastProvider>
    </StoreProvider>,
  )
  return screen.getByRole('dialog')
}

const accept = (dialog: HTMLElement) => within(dialog).queryByRole('button', { name: /^Accept / })
const typeWord = (dialog: HTMLElement, word = 'confirm') =>
  fireEvent.change(within(dialog).getByRole('textbox', { name: 'Type confirm to accept the removals' }), { target: { value: word } })

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the confirmation dialog', () => {
  test('every item of the list, the offer, what it does not affect, and the word to type', async () => {
    stubApply(() => golden)
    const { store } = await stream(golden)
    const dialog = show(store)
    for (const item of ['a.example.com', 'b.example.com', 'qemu/104 db-1', 'lxc/200', zone.detail, tunnel.detail]) {
      expect(within(dialog).getByText(item)).toBeTruthy()
    }
    expect(within(dialog).getByText(golden.offer ?? '')).toBeTruthy()
    // the delete of old.example.com is held by its grace period; no removal offered names it
    expect(within(dialog).getByText('Destructive actions that are pending; a confirmation does not affect them:')).toBeTruthy()
    expect(within(dialog).getByText('old.example.com')).toBeTruthy()
    const button = accept(dialog) as HTMLElement
    expect(button.textContent).toBe('Accept 2 removals, 2 vanished guests, 1 zone that is gone and 1 tunnel that is gone at the next run')
    expect(button.getAttribute('aria-disabled')).toBe('true')
    typeWord(dialog, 'yes')
    expect(button.getAttribute('aria-disabled')).toBe('true')
    typeWord(dialog)
    expect(accept(dialog)?.getAttribute('aria-disabled')).toBeNull()
  })

  test('a new offer from the stream while the admin reads: what changed, and the old offer is never sent', async () => {
    const { store, deliver, now } = await stream(golden)
    const sent = stubApply(now)
    const dialog = show(store)
    typeWord(dialog)
    expect(accept(dialog)?.getAttribute('aria-disabled')).toBeNull()

    await deliver(changed)
    expect(within(dialog).getByText('What waits changed while you were reading.')).toBeTruthy()
    expect(accept(dialog)).toBeNull()
    expect(within(dialog).getByText('c.example.com').closest('li')?.textContent).toBe('added c.example.com')
    expect(within(dialog).getByText(tunnel.detail).closest('li')?.textContent).toBe(`removed ${tunnel.detail}`)
    expect(sent).toEqual([])

    fireEvent.click(within(dialog).getByRole('button', { name: 'Review the new list' }))
    expect(within(dialog).getByText(newOffer)).toBeTruthy()
    expect(within(dialog).getByText('c.example.com')).toBeTruthy()
    // the word is typed again for the new list
    expect(accept(dialog)?.getAttribute('aria-disabled')).toBe('true')
    typeWord(dialog)
    fireEvent.click(accept(dialog) as HTMLElement)
    expect(await within(dialog).findByText('Confirmed for the next run:')).toBeTruthy()
    expect(sent).toEqual([{ confirmDeletes: true, offer: newOffer }])
  })

  test('a refusal of the daemon: its sentence, and the new list to review', async () => {
    let offered = golden
    const sent = stubApply(() => offered)
    const { store } = await stream(golden)
    const dialog = show(store)
    typeWord(dialog)
    // another admin's change that the stream has not brought yet
    offered = changed
    fireEvent.click(accept(dialog) as HTMLElement)
    expect(await within(dialog).findByText('What waits changed while you were reading.')).toBeTruthy()
    expect(within(dialog).getByText(/^The daemon says:/).textContent).toBe(
      'The daemon says: refused: what waits for a confirmation changed since it was shown; look again and repeat',
    )
    expect(sent).toEqual([{ confirmDeletes: true, offer: golden.offer }])
    expect(within(dialog).getByRole('button', { name: 'Review the new list' })).toBeTruthy()
  })

  test('accepted: what was confirmed, and that publishing has started', async () => {
    const zoneOnly: State = { ...golden, waiting: [zone], digest: '2222222222222222' }
    const sent = stubApply(() => zoneOnly, true)
    const { store } = await stream({ ...zoneOnly, mode: 'observe' })
    const dialog = show(store)
    expect(within(dialog).getByText('pco observes only: a confirmation starts publishing as well.')).toBeTruthy()
    // no word for a zone that is gone
    expect(within(dialog).queryByRole('textbox')).toBeNull()
    fireEvent.click(accept(dialog) as HTMLElement)
    expect(await within(dialog).findByText('Confirmed for the next run:')).toBeTruthy()
    expect(within(dialog).getByText(zone.detail)).toBeTruthy()
    expect(within(dialog).getByText('Publishing has started: the daemon changes Cloudflare from the next cycle.')).toBeTruthy()
    expect(sent).toEqual([{ confirmDeletes: true, offer: golden.offer }])
  })

  test('a daemon busy with a cycle: the list stays, with the word, and what went wrong', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => json({ error: 'a cycle is running; try again', code: 'unavailable' }, 503)),
    )
    const { store } = await stream(golden)
    const dialog = show(store)
    typeWord(dialog)
    fireEvent.click(accept(dialog) as HTMLElement)
    expect((await within(dialog).findByRole('alert')).textContent).toBe('a cycle is running; try again')
    expect((within(dialog).getByRole('textbox') as HTMLInputElement).value).toBe('confirm')
    expect(accept(dialog)?.getAttribute('aria-disabled')).toBeNull()
  })
})
