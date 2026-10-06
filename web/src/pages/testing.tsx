// What the tests of the pages share: pco web's answers to the calls a page
// makes itself, and the providers a page renders in.

import { render } from '@testing-library/react'
import type { ReactNode } from 'react'
import { vi } from 'vitest'

import { type AppStore, StoreProvider } from '../api/store'
import { ToastProvider } from '../components/Toast'

export interface Sent {
  method: string
  path: string
  body?: unknown
}

export interface Reply {
  status: number
  body?: unknown
}

// stubApi answers the calls of the page by "METHOD /path", the query left
// out, and keeps every call in the order it was made; a call it has no answer
// for is not found. An answer that is a promise keeps the call running until
// it settles.
export function stubApi(answers: Record<string, Reply | ((s: Sent) => Reply | Promise<Reply>)>): Sent[] {
  const sent: Sent[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init?: RequestInit) => {
      const s: Sent = { method: init?.method ?? 'GET', path: input, body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined }
      sent.push(s)
      const answer = answers[`${s.method} ${s.path.split('?')[0]}`]
      const r = await (typeof answer === 'function' ? answer(s) : (answer ?? { status: 404, body: { error: `no answer in the test for ${s.method} ${s.path}`, code: 'not_found' } }))
      return new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status })
    }),
  )
  return sent
}

// writes are the calls that would change something.
export const writes = (sent: readonly Sent[]) => sent.filter((s) => s.method !== 'GET')

export function renderPage(store: AppStore, ui: ReactNode) {
  return render(
    <StoreProvider store={store}>
      <ToastProvider>{ui}</ToastProvider>
    </StoreProvider>,
  )
}
