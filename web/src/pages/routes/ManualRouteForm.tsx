import { type FormEvent, type ReactNode, useEffect, useState } from 'react'

import { api, ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { GuestListView, ManualRouteView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { navigate } from '../../app/router'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Empty } from '../../components/Empty'
import { RefreshIcon } from '../../components/icons'
import { Field } from '../../components/Field'
import { Skeleton } from '../../components/Skeleton'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { marked } from '../../text/chars'
import { bodyOf, fieldOfPath, type ManualErrors, type ManualField, type ManualValues, needsNodeQuestion, normalHostname, serviceOf, validate, valuesOf } from './manual'
import { ErrorText, errorMessage, NoteLine, PageHead, readerReason, RoutesNav, useAdmin } from './parts'
import { manualPrefix, ownerName, routeLink } from './routes'

// heldBy is the owner that holds a hostname now, when that is not the route
// being edited: the manual route then competes for it like any owner.
function useHeldBy(hostname: string, self: string | undefined): string | undefined {
  const routes = useApp((s) => s.state?.routes)
  const host = normalHostname(hostname)
  const holder = (routes ?? []).find((r) => r.hostname === host && r.state !== 'conflict')
  if (!holder || holder.owner === self) return undefined
  return ownerName(holder.owner, holder.guest)
}

// useGuests reads the guests for the picker, or why they could not be read.
function useGuests(): GuestListView[] | ApiError | undefined {
  const [guests, setGuests] = useState<GuestListView[] | ApiError>()
  useEffect(() => {
    let on = true
    api<GuestListView[]>('GET', '/api/v1/guests', undefined, { background: true }).then(
      (g) => on && setGuests(g ?? []),
      (e: unknown) => on && setGuests(e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: String(e) })),
    )
    return () => {
      on = false
    }
  }, [])
  return guests
}

export interface ManualRouteFormProps {
  // The route as it was read, with its revision; none for a new one.
  route?: ManualRouteView
  readOnly?: boolean
  onSaved: (v: ManualRouteView) => void
  onDeleted: () => void
  // Reads the route again after the daemon refused a stale revision.
  onLookAgain: () => void
}

// ManualRouteForm makes or changes a manual route: a hostname for a guest or
// an address that no annotation names. A change carries the revision it was
// read at; a route that changed since is refused, and read again.
export function ManualRouteForm({ route, readOnly, onSaved, onDeleted, onLookAgain }: ManualRouteFormProps) {
  const isNew = route === undefined
  const toast = useToast()
  const manualCIDRs = useApp((s) => (s.settings ? (s.settings.settings.manualCIDRs ?? []) : undefined))
  const guests = useGuests()
  const [v, setV] = useState<ManualValues>(() => valuesOf(route))
  const [errors, setErrors] = useState<ManualErrors>({})
  const [failure, setFailure] = useState<unknown>()
  const [sending, setSending] = useState(false)
  const [asking, setAsking] = useState<'node' | 'delete'>()
  const self = route ? `${manualPrefix}${route.id}` : v.id ? `${manualPrefix}${v.id}` : undefined
  const heldBy = useHeldBy(v.hostname, self)

  const set = <K extends ManualField>(k: K, value: ManualValues[K]) => {
    setV((now) => ({ ...now, [k]: value }))
    if (errors[k]) setErrors((now) => ({ ...now, [k]: undefined }))
  }

  const fail = (e: unknown) => {
    if (e instanceof ApiError && e.code === 'invalid' && e.field) {
      const field = Object.hasOwn(fieldOfPath, e.field) ? fieldOfPath[e.field] : undefined
      if (field) {
        setErrors((now) => ({ ...now, [field]: e.message }))
        return
      }
    }
    setFailure(e)
  }

  const save = async () => {
    setSending(true)
    setFailure(undefined)
    try {
      const body = bodyOf(v, isNew, route?.rev)
      const saved = isNew
        ? await api<ManualRouteView>('POST', '/api/v1/routes/manual', body)
        : await api<ManualRouteView>('PUT', `/api/v1/routes/manual/${encodeURIComponent(route.id)}`, body)
      toast(
        <>
          Manual route <Untrusted text={`${manualPrefix}${saved.id}`} /> saved; the next cycle, which has been asked for, publishes it.
        </>,
        'ok',
      )
      onSaved(saved)
    } catch (e) {
      fail(e)
    } finally {
      setSending(false)
    }
  }

  const remove = async () => {
    if (!route) return
    setSending(true)
    setFailure(undefined)
    try {
      await api('DELETE', `/api/v1/routes/manual/${encodeURIComponent(route.id)}?rev=${route.rev}`)
      toast(
        <>
          Manual route <Untrusted text={`${manualPrefix}${route.id}`} /> removed.
        </>,
        'ok',
      )
      onDeleted()
    } catch (e) {
      setAsking(undefined)
      setFailure(e)
    } finally {
      setSending(false)
    }
  }

  const submit = (e: FormEvent) => {
    e.preventDefault()
    const found = validate(v, { isNew, manualCIDRs })
    setErrors(found)
    if (Object.values(found).some(Boolean)) return
    if (needsNodeQuestion(v, route)) setAsking('node')
    else void save()
  }

  const refused = failure instanceof ApiError && failure.code === 'refused'
  const prefixes =
    manualCIDRs === undefined ? (
      'The settings say which prefixes manual routes may use.'
    ) : manualCIDRs.length > 0 ? (
      <>Allowed prefixes (manualCIDRs): {manualCIDRs.join(', ')}.</>
    ) : (
      <>
        No prefix is allowed yet: add one to manualCIDRs in the <Link to="/settings">settings</Link> first.
      </>
    )
  const guestList = Array.isArray(guests) ? guests : []
  const listed = guestList.some((g) => g.ref === v.guest)
  const guestHint =
    guests === undefined ? (
      'Loading the guests…'
    ) : guests instanceof ApiError ? (
      <Untrusted text={`The guests could not be read: ${errorMessage(guests)}`} />
    ) : undefined

  const text = (k: 'id' | 'hostname' | 'addr' | 'port' | 'hostHeader' | 'sni' | 'via', label: ReactNode, hint?: ReactNode, mono = true) => (
    <Field label={label} hint={hint} error={errors[k]}>
      {(c) => (
        <input
          {...c}
          type="text"
          className={mono ? 'mono' : undefined}
          value={v[k]}
          autoComplete="off"
          spellCheck={false}
          disabled={readOnly}
          onChange={(e) => set(k, e.target.value)}
        />
      )}
    </Field>
  )

  return (
    <form className="manual-form" onSubmit={submit} noValidate>
      {readOnly && <p className="muted">Only an admin changes manual routes: {readerReason}.</p>}
      {isNew && text('id', 'Id', 'a-z, 0-9 and -, up to 32; left empty, pco gives it one. The route owns its hostname as manual/<id>.')}
      {text('hostname', 'Hostname', undefined)}
      {heldBy && (
        <NoteLine>
          <Untrusted text={normalHostname(v.hostname)} hostname /> is held by <Untrusted text={heldBy} /> now: this route competes for it in the claims like
          any owner, and serves it only once it holds it.
        </NoteLine>
      )}
      <fieldset className="choices" disabled={readOnly}>
        <legend>Target</legend>
        <label className="choice">
          <input type="radio" name="kind" checked={v.kind === 'guest'} onChange={() => set('kind', 'guest')} /> a guest, at the address pco proves for it
        </label>
        <label className="choice">
          <input type="radio" name="kind" checked={v.kind === 'address'} onChange={() => set('kind', 'address')} /> an address
        </label>
      </fieldset>
      {v.kind === 'guest' ? (
        <>
          <Field label="Guest" error={errors.guest} hint={guestHint}>
            {(c) => (
              <select {...c} value={v.guest} disabled={readOnly} onChange={(e) => set('guest', e.target.value)}>
                <option value="">Choose a guest</option>
                {!listed && v.guest && <option value={v.guest}>{marked(v.guest)}</option>}
                {guestList.map((g) => (
                  <option key={g.ref} value={g.ref}>
                    {marked(g.name ? `${g.ref} ${g.name}` : g.ref)}
                  </option>
                ))}
              </select>
            )}
          </Field>
          {text('via', 'Via (optional)', 'the NIC, as net0, or the address of the guest to reach it by')}
        </>
      ) : (
        <>
          {text('addr', 'Address', prefixes)}
          <label className="check">
            <input type="checkbox" checked={v.allowNode} disabled={readOnly} onChange={(e) => set('allowNode', e.target.checked)} /> allowNode: the address is a
            node of the cluster, and this publishes one of its services
          </label>
        </>
      )}
      <div className="form-row">
        <Field label="Scheme" error={errors.scheme}>
          {(c) => (
            <select {...c} value={v.scheme} disabled={readOnly} onChange={(e) => set('scheme', e.target.value === 'https' ? 'https' : 'http')}>
              <option value="http">http</option>
              <option value="https">https</option>
            </select>
          )}
        </Field>
        {text('port', 'Port', undefined)}
      </div>
      {text('hostHeader', 'Host header (optional)', 'sent to the target instead of the hostname')}
      {v.scheme === 'https' && (
        <>
          {text('sni', 'SNI (optional)', 'the name the target’s certificate is checked against')}
          <label className="check">
            <input type="checkbox" checked={v.noTLSVerify} disabled={readOnly} onChange={(e) => set('noTLSVerify', e.target.checked)} /> noTLSVerify: do not
            check the target’s certificate
          </label>
        </>
      )}
      {failure !== undefined && <ErrorText error={failure} />}
      {refused && !isNew && <p className="muted">Look again reads the route as it is now: the changes made here are dropped.</p>}
      {!readOnly && (
        <div className="form-actions">
          <Button type="submit" variant="primary" disabledReason={sending ? 'it is being sent' : undefined}>
            {isNew ? 'Make the route' : 'Save'}
          </Button>
          {refused && !isNew && (
            <Button icon={<RefreshIcon />} onClick={onLookAgain}>
              Look again
            </Button>
          )}
          {!isNew && (
            <Button variant="danger" onClick={() => setAsking('delete')}>
              Delete
            </Button>
          )}
          <Link to="/routes" className="btn">
            Cancel
          </Link>
        </div>
      )}
      {asking === 'node' && (
        <Dialog
          open
          onClose={() => setAsking(undefined)}
          title="Publish a service of a node"
          footer={
            <>
              <Button onClick={() => setAsking(undefined)}>Cancel</Button>
              <Button
                variant="danger"
                onClick={() => {
                  setAsking(undefined)
                  void save()
                }}
              >
                Publish {serviceOf(v)}
              </Button>
            </>
          }
        >
          <p>
            <span className="mono">{serviceOf(v)}</span> is a service of a node of the cluster. Whoever reaches{' '}
            <Untrusted text={normalHostname(v.hostname)} hostname /> reaches that service on the node.
          </p>
        </Dialog>
      )}
      {asking === 'delete' && route && (
        <Dialog
          open
          onClose={() => setAsking(undefined)}
          title={
            <>
              Delete <Untrusted text={`${manualPrefix}${route.id}`} />
            </>
          }
          footer={
            <>
              <Button onClick={() => setAsking(undefined)}>Cancel</Button>
              <Button variant="danger" onClick={() => void remove()} disabledReason={sending ? 'it is being sent' : undefined}>
                Delete the route
              </Button>
            </>
          }
        >
          <p>
            pco stops publishing <Untrusted text={route.hostname} hostname /> for this route from the next cycle; the claim on it goes to the next owner that waits.
          </p>
        </Dialog>
      )}
    </form>
  )
}

// ManualRoutePage is /routes/manual/new and /routes/manual/:id.
export function ManualRoutePage({ id }: { id?: string }) {
  const admin = useAdmin()
  const [routes, setRoutes] = useState<ManualRouteView[] | ApiError>()
  const [asked, setAsked] = useState(0)

  useEffect(() => {
    if (id === undefined) return
    let on = true
    api<ManualRouteView[]>('GET', '/api/v1/routes/manual', undefined, { background: true }).then(
      (r) => on && setRoutes(r ?? []),
      (e: unknown) => on && setRoutes(e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: String(e) })),
    )
    return () => {
      on = false
    }
  }, [id, asked])

  const head = (
    <>
      <PageHead
        title={
          id === undefined ? (
            'New manual route'
          ) : (
            <>
              Manual route <Untrusted text={id} />
            </>
          )
        }
        description="A hostname for a guest or an address that no annotation names. It owns its hostname as manual/<id>."
      />
      <RoutesNav current="routes" />
    </>
  )
  const done = (v: ManualRouteView) => {
    if (v.id === id) setAsked(asked + 1)
    else navigate(`/routes/manual/${encodeURIComponent(v.id)}`)
  }
  if (id === undefined) {
    return (
      <>
        {head}
        <ManualRouteForm readOnly={!admin} onSaved={done} onDeleted={() => navigate('/routes')} onLookAgain={() => undefined} />
      </>
    )
  }
  if (routes === undefined) {
    return (
      <>
        {head}
        <Skeleton lines={6} label="Loading the manual route" />
      </>
    )
  }
  if (routes instanceof ApiError) {
    return (
      <>
        {head}
        <ErrorText error={routes} />
      </>
    )
  }
  const route = routes.find((r) => r.id === id)
  if (!route) {
    return (
      <>
        {head}
        <Empty title="There is no such manual route">
          <p>
            <Link to="/routes/manual/new">Make a new manual route</Link>
          </p>
        </Empty>
      </>
    )
  }
  return (
    <>
      {head}
      <p>
        <Link to={routeLink(route.hostname, `${manualPrefix}${route.id}`)}>Its route, and what pco made of it</Link>
      </p>
      <ManualRouteForm
        key={route.rev}
        route={route}
        readOnly={!admin}
        onSaved={done}
        onDeleted={() => navigate('/routes')}
        onLookAgain={() => setAsked(asked + 1)}
      />
    </>
  )
}
