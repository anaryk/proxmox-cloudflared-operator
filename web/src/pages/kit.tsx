// What the pages of guests and of the edge share: reading an answer of the
// daemon when a page opens, saying an error with what to do about it, and
// the reason an admin action is refused to a reader.

import './kit.css'

import { type ReactNode, useCallback, useEffect, useState } from 'react'

import { api, ApiError } from '../api/client'
import { explain } from '../api/errors'
import { useApp } from '../api/store'
import type { GuestView, State } from '../api/types.gen'
import { Button } from '../components/Button'
import { Untrusted } from '../components/Untrusted'

// The reason every admin action gives a reader: the privilege of an admin.
export const readerReason = 'needs Sys.Modify on /'

// useAdminReason is why this user may not take an admin action, or
// undefined for an admin.
export function useAdminReason(): string | undefined {
  const role = useApp((s) => s.session?.role)
  return role === 'admin' ? undefined : readerReason
}

export const asApiError = (e: unknown): ApiError => (e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: String(e) }))

export interface Resource<T> {
  data?: T
  error?: ApiError
  // The answer for this path is not there yet; what was there before stays.
  loading: boolean
  reload: () => void
}

interface Got<T> {
  path: string
  round: number
  data?: T
  error?: ApiError
}

// useResource reads path when the page opens it, again when reload is
// called and when again changes, as with the digest of the state. What it
// read before stays shown until the new answer comes.
export function useResource<T>(path: string | undefined, again?: unknown): Resource<T> {
  const [round, setRound] = useState(0)
  const [got, setGot] = useState<Got<T>>()

  useEffect(() => {
    if (!path) return
    let live = true
    api<T>('GET', path).then(
      (data) => {
        if (live) setGot({ path, round, data })
      },
      (e: unknown) => {
        if (live) setGot((before) => ({ path, round, data: before?.path === path ? before.data : undefined, error: asApiError(e) }))
      },
    )
    return () => {
      live = false
    }
  }, [path, round, again])

  const reload = useCallback(() => setRound((r) => r + 1), [])
  const mine = got?.path === path ? got : undefined
  return { data: mine?.data, error: mine?.error, loading: path !== undefined && mine?.round !== round, reload }
}

// Failure says what went wrong in the page's words, or the daemon's as
// untrusted text, and offers what fits: to look again at what changed, or to
// try again.
export function Failure({ error, onLookAgain, onTryAgain }: { error: ApiError; onLookAgain?: () => void; onTryAgain?: () => void }) {
  const e = explain(error)
  return (
    <div className="failure" role="alert">
      <p className="form-error">{e.quoted ? <Untrusted text={e.text} /> : e.text}</p>
      {e.remedy === 'look-again' && onLookAgain && (
        <Button small onClick={onLookAgain}>
          Look again
        </Button>
      )}
      {e.remedy === 'try-again' && onTryAgain && (
        <Button small onClick={onTryAgain}>
          Try again
        </Button>
      )}
      {e.remedy === 'reload' && (
        <Button small onClick={() => window.location.reload()}>
          Reload
        </Button>
      )}
    </div>
  )
}

// Card is a section of a page with its heading.
export function Card({ id, title, actions, children }: { id: string; title: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="card" aria-labelledby={id}>
      <div className="card-head">
        <h2 id={id}>{title}</h2>
        {actions && <div className="card-actions">{actions}</div>}
      </div>
      <div className="card-body">{children}</div>
    </section>
  )
}

// Owner names a guest or a manual route as engine.OwnerName does, "qemu/101
// (web-1)"; the name is the guest's own, so it is untrusted text.
export function Owner({ owner, guest }: { owner: string; guest?: Pick<GuestView, 'name'> | null }) {
  if (!guest?.name) return <Untrusted text={owner} />
  return (
    <>
      <Untrusted text={owner} /> (<Untrusted text={guest.name} />)
    </>
  )
}

// ownerText is the same as text, for a sentence that is untrusted as a whole.
export const ownerText = (owner: string, guest?: Pick<GuestView, 'name'> | null) => (guest?.name ? `${owner} (${guest.name})` : owner)

// accountName is the name of an account as the credentials' last checks
// list it, or its id when none does.
export function accountName(st: Pick<State, 'credentials'> | undefined, id: string): string {
  for (const c of st?.credentials ?? []) {
    const found = c.report?.accounts.find((a) => a.id === id)
    if (found?.name) return found.name
  }
  return id
}

// credentialLabel is the label of a credential of the state, or its id.
export function credentialLabel(st: Pick<State, 'credentials'> | undefined, id: string): string {
  return st?.credentials.find((c) => c.id === id)?.label || id
}

// waitingId is the id of the element of the Plan that shows one entry of
// what waits; the confirmation of the daemon's offer happens there.
export const waitingId = (kind: string, subject: string) => `waiting-${kind}-${subject}`

// planEntry is the link to the Plan with one entry of what waits picked out:
// the shell gives the element the fragment names the focus.
export function planEntry(kind: string, subject: string): string {
  return `/routes/plan#${encodeURIComponent(waitingId(kind, subject))}`
}

// Go writes a time it never set as the zero time, or leaves it out.
export const unset = (at?: string) => !at || at.startsWith('0001-01-01T00:00:00')
