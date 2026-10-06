import type { ReactNode } from 'react'

import { ApiError } from '../../api/client'
import { explain } from '../../api/errors'
import { useApp } from '../../api/store'
import type { GuestView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { FailIcon, InfoIcon, WarnIcon } from '../../components/icons'
import { Untrusted } from '../../components/Untrusted'

// Why a reader's button does nothing, said next to it.
export const readerReason = 'needs Sys.Modify on /'

export function useAdmin(): boolean {
  return useApp((s) => s.session?.role === 'admin')
}

export function PageHead({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="page-head">
      <div>
        <h1>{title}</h1>
        {description && <p className="page-description">{description}</p>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  )
}

// RoutesNav switches between the routes and their plan.
export function RoutesNav({ current }: { current: 'routes' | 'plan' }) {
  const item = (to: string, id: 'routes' | 'plan', label: string) => (
    <Link to={to} className="subnav-link" aria-current={current === id ? 'page' : undefined}>
      {label}
    </Link>
  )
  return (
    <nav className="subnav" aria-label="Routes and plan">
      {item('/routes', 'routes', 'Routes')}
      {item('/routes/plan', 'plan', 'Plan')}
    </nav>
  )
}

// Owner names a guest by its name and its reference, a manual route by its
// reference alone.
export function Owner({ owner, guest }: { owner: string; guest?: GuestView }) {
  return (
    <span className="owner">
      {guest?.name && (
        <>
          <Untrusted text={guest.name} />{' '}
        </>
      )}
      <span className="mono">
        <Untrusted text={owner} />
      </span>
    </span>
  )
}

// ErrorText says what went wrong in the page's words, or in the daemon's,
// which may quote a guest, as untrusted text.
export function ErrorText({ error }: { error: unknown }) {
  const e = error instanceof ApiError ? error : new ApiError(0, { code: 'internal', error: String(error) })
  const said = explain(e)
  return (
    <p className="error-line" role="alert">
      <FailIcon />
      <span>{said.quoted ? <Untrusted text={said.text} /> : said.text}</span>
    </p>
  )
}

export function errorMessage(error: unknown): string {
  const e = error instanceof ApiError ? error : new ApiError(0, { code: 'internal', error: String(error) })
  return explain(e).text
}

// NoteLine says something about what follows it, with an icon of its tone.
export function NoteLine({ tone = 'warn', status, children }: { tone?: 'info' | 'warn'; status?: boolean; children: ReactNode }) {
  return (
    <p className={tone === 'warn' ? 'note-line note-warn' : 'note-line'} role={status ? 'status' : undefined}>
      {tone === 'warn' ? <WarnIcon /> : <InfoIcon />}
      <span>{children}</span>
    </p>
  )
}
