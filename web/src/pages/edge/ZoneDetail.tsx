import { type JSX, useState } from 'react'

import { api, type ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { RouteView, SettingsView, State, ZoneView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { FrozenIcon, InfoIcon, NoZoneIcon, OkIcon } from '../../components/icons'
import { Skeleton } from '../../components/Skeleton'
import { StateBadge, StatusBadge } from '../../components/StateBadge'
import { type Column, Table } from '../../components/Table'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { asApiError, accountName, Card, credentialLabel, Failure, Owner, planEntry, useAdminReason } from '../kit'
import { CheckLine } from './Checklist'

// ZoneState is the state of a zone as the last cycle used it.
export function ZoneState({ state }: { state: string }) {
  switch (state) {
    case 'served':
      return (
        <StatusBadge tone="ok" icon={<OkIcon />}>
          served
        </StatusBadge>
      )
    case 'frozen':
      return (
        <StatusBadge tone="warn" icon={<FrozenIcon />}>
          frozen
        </StatusBadge>
      )
    case 'left out':
    case 'not served':
      return (
        <StatusBadge tone="idle" icon={<NoZoneIcon />}>
          {state}
        </StatusBadge>
      )
  }
  return (
    <StatusBadge tone="idle" icon={<InfoIcon />}>
      <Untrusted text={state} />
    </StatusBadge>
  )
}

// CredentialName is a credential by its label, with its id when they differ.
export function CredentialName({ st, id }: { st: Pick<State, 'credentials'> | undefined; id: string }) {
  const label = credentialLabel(st, id)
  return label === id ? <Untrusted text={id} /> : <Untrusted text={`${label} (${id})`} />
}

export function CredentialNames({ st, ids }: { st: Pick<State, 'credentials'> | undefined; ids: readonly string[] }) {
  if (ids.length === 0) return <>-</>
  return (
    <>
      {ids.map((id, at) => (
        <span key={id}>
          {at > 0 && ', '}
          <CredentialName st={st} id={id} />
        </span>
      ))}
    </>
  )
}

// leftOutBy are the credentials that leave a zone out, each with why and
// what to grant, from the reports of their last checks.
export function leftOutBy(st: Pick<State, 'credentials'> | undefined, z: ZoneView): { credential: string; reason: string; detail: string }[] {
  return z.excluded.map((id) => {
    const x = st?.credentials.find((c) => c.id === id)?.report?.excluded.find((e) => e.zoneId === z.id || e.zone === z.name)
    return { credential: id, reason: x?.reason ?? '', detail: x?.detail ?? '' }
  })
}

// staleEntry is the entry of what waits that lets a stale or refused zone
// go, when there is one.
export const staleEntry = (st: Pick<State, 'waiting'> | undefined, zone: string) => st?.waiting.find((w) => w.kind === 'stale-zone' && w.subject === zone)

export function ConfirmLink({ zone }: { zone: string }) {
  return <Link to={planEntry('stale-zone', zone)}>Confirm in the plan</Link>
}

// pinsAfter are the pins of the settings with zone pinned to credential, or
// without its pin for "".
export function pinsAfter(s: SettingsView['settings'], zone: string, credential: string): Record<string, string> {
  const pins = Object.entries(s.zonePins ?? {}).filter(([z]) => z !== zone)
  if (credential) pins.push([zone, credential])
  return Object.fromEntries(pins)
}

// PinDialog pins a zone to one of the credentials that list it, or takes
// its pin off: a change of the settings, saved with the revision they were
// read at. Settings changed in between are refused, and read again.
export function PinDialog({ zone, open, onClose }: { zone: ZoneView; open: boolean; onClose: () => void }) {
  const toast = useToast()
  const st = useApp((s) => s.state)
  const loaded = useApp((s) => s.settings)
  const [fresh, setFresh] = useState<SettingsView>()
  const settings = fresh && (!loaded || fresh.rev >= loaded.rev) ? fresh : loaded
  const now = settings?.settings.zonePins?.[zone.name] ?? ''
  const [picked, setPicked] = useState<string>()
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()
  const choice = picked ?? now

  const close = () => {
    setPicked(undefined)
    setError(undefined)
    onClose()
  }

  const save = async () => {
    if (!settings) return
    setSending(true)
    setError(undefined)
    try {
      await api('PUT', '/api/v1/settings', { rev: settings.rev, settings: { ...settings.settings, zonePins: pinsAfter(settings.settings, zone.name, choice) } })
      toast(<Untrusted text={choice ? `Zone ${zone.name} is pinned to ${credentialLabel(st, choice)}.` : `Zone ${zone.name} is pinned to no credential.`} />, 'ok')
      setPicked(undefined)
      onClose()
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  const lookAgain = async () => {
    setError(undefined)
    setPicked(undefined)
    try {
      setFresh(await api<SettingsView>('GET', '/api/v1/settings'))
    } catch (e) {
      setError(asApiError(e))
    }
  }

  const options = [...new Set([...zone.credentials, ...(now ? [now] : [])])]
  return (
    <Dialog
      open={open}
      onClose={close}
      title={`Pin ${zone.name}`}
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="primary" disabled={sending || !settings || choice === now} onClick={() => void save()}>
            Save the pin
          </Button>
        </>
      }
    >
      <p>
        A pin gives the zone to one credential when several list it, and keeps it there; the setting is <span className="mono">zonePins</span>.
      </p>
      {!settings ? (
        <Skeleton lines={2} label="Loading the settings" />
      ) : (
        <fieldset className="radio-list">
          <legend>
            Pin <Untrusted text={zone.name} hostname /> to
          </legend>
          {options.map((id) => (
            <label key={id}>
              <input type="radio" name="zone-pin" value={id} checked={choice === id} onChange={() => setPicked(id)} />
              <CredentialName st={st} id={id} />
              {id === now && <span className="muted"> (pinned now)</span>}
            </label>
          ))}
          <label>
            <input type="radio" name="zone-pin" value="" checked={choice === ''} onChange={() => setPicked('')} />
            no credential
            {now === '' && <span className="muted"> (no pin now)</span>}
          </label>
        </fieldset>
      )}
      {settings && choice !== now && (
        <p>
          {choice ? (
            <>
              zonePins: <Untrusted text={zone.name} hostname /> to <CredentialName st={st} id={choice} />
              {now && (
                <>
                  , not <CredentialName st={st} id={now} />
                </>
              )}
            </>
          ) : (
            <>
              zonePins: no pin for <Untrusted text={zone.name} hostname />
            </>
          )}
          , at revision {settings.rev} of the settings.
        </p>
      )}
      {sending && <Busy label="Saving the settings" />}
      {error && <Failure error={error} onLookAgain={() => void lookAgain()} onTryAgain={() => void save()} />}
    </Dialog>
  )
}

function ZoneRoutes({ routes, compact }: { routes: RouteView[]; compact: boolean }) {
  const columns: Column<RouteView>[] = [
    {
      key: 'hostname',
      header: 'Hostname',
      lead: true,
      cell: (r) => (
        <Link to={`/routes/${encodeURIComponent(r.hostname)}?owner=${encodeURIComponent(r.owner)}`}>
          <Untrusted text={r.hostname} hostname />
        </Link>
      ),
    },
    { key: 'owner', header: 'Owner', cell: (r) => <Owner owner={r.owner} guest={r.guest} /> },
    { key: 'state', header: 'State', cell: (r) => <StateBadge state={r.state} /> },
  ]
  return <Table label="Its routes" columns={columns} rows={routes} rowKey={(r) => `${r.hostname} ${r.owner}`} height={compact ? 200 : 360} empty="No route is in this zone." />
}

// ZoneDetail is everything about one zone as the last cycle used it: its
// state and why, which credentials list it, serve it and leave it out, its
// pin, what the credentials' checks found of it, and its routes. It is the
// page of the zone and the zone's drawer of the map.
export function ZoneDetail({ zone, variant }: { zone: string; variant: 'drawer' | 'page' }): JSX.Element {
  const st = useApp((s) => s.state)
  const refusal = useAdminReason()
  const [pinning, setPinning] = useState(false)
  if (!st) return <Skeleton lines={4} label="Loading the state" />
  const z = st.zones.find((x) => x.name === zone)
  if (!z) return <p className="muted">The last cycle used no zone <Untrusted text={zone} hostname />.</p>
  const stale = staleEntry(st, z.name)
  const left = leftOutBy(st, z)
  const routes = st.routes.filter((r) => r.zone === z.name)
  const conflicts = st.conflicts.filter((c) => c.zone === z.name)
  const checks = st.credentials.flatMap((c) =>
    (c.report?.checks ?? []).filter((k) => k.scopeId === z.id || (!k.scopeId && k.scope === z.name)).map((k) => ({ credential: c.id, check: k })),
  )
  const compact = variant === 'drawer'

  return (
    <div className={compact ? 'detail detail-drawer' : 'detail'}>
      <Card
        id={`zone-${z.id}`}
        title={<Untrusted text={z.name} hostname />}
        actions={
          <Button small disabledReason={refusal} onClick={() => setPinning(true)}>
            Pin …
          </Button>
        }
      >
        <dl className="details">
          <dt>State</dt>
          <dd>
            <ZoneState state={z.state} />
          </dd>
          {z.frozenWhy && (
            <>
              <dt>Frozen because</dt>
              <dd>
                <Untrusted text={z.frozenWhy} />
              </dd>
            </>
          )}
          <dt>Account</dt>
          <dd>
            <Untrusted text={accountName(st, z.accountId)} />
            {accountName(st, z.accountId) !== z.accountId && (
              <span className="muted mono">
                {' '}
                <Untrusted text={z.accountId} />
              </span>
            )}
          </dd>
          <dt>At Cloudflare</dt>
          <dd>
            <Untrusted text={z.status || '-'} />
          </dd>
          <dt>Listed by</dt>
          <dd>
            <CredentialNames st={st} ids={z.credentials} />
          </dd>
          <dt>Served by</dt>
          <dd>{z.servedBy ? <CredentialName st={st} id={z.servedBy} /> : '-'}</dd>
          <dt>Pinned to</dt>
          <dd>{z.pinned ? <CredentialName st={st} id={z.pinned} /> : '-'}</dd>
          {z.stale.length > 0 && (
            <>
              <dt>No longer listed by</dt>
              <dd>
                <CredentialNames st={st} ids={z.stale} />
              </dd>
            </>
          )}
          <dt>Records in the way</dt>
          <dd>{conflicts.length === 0 ? 'none' : <Untrusted text={conflicts.map((c) => `${c.name} ${c.type} ${c.content}`).join('; ')} />}</dd>
        </dl>
        {stale && (
          <div className="callout">
            <p>
              <Untrusted text={stale.detail} />
            </p>
            <ConfirmLink zone={z.name} />
          </div>
        )}
      </Card>
      {(checks.length > 0 || left.length > 0) && (
        <Card id={`zone-checks-${z.id}`} title="What the credentials' checks found">
          <ul className="checks">
            {checks.map(({ credential, check }, at) => (
              <CheckLine key={`${credential}:${at}`} check={check} by={credentialLabel(st, credential)} />
            ))}
            {left.map((x) => (
              <li key={x.credential} className="check check-left-out">
                <span className="check-mark" aria-hidden="true">
                  -
                </span>
                <span className="check-name">
                  <Untrusted text={`${credentialLabel(st, x.credential)}: ${z.name} left out${x.reason ? `: ${x.reason}` : ''}`} />
                </span>
                {x.detail && (
                  <span className="check-detail">
                    <Untrusted text={x.detail} />
                  </span>
                )}
              </li>
            ))}
          </ul>
        </Card>
      )}
      <Card id={`zone-routes-${z.id}`} title="Routes">
        <ZoneRoutes routes={routes} compact={compact} />
      </Card>
      <PinDialog zone={z} open={pinning} onClose={() => setPinning(false)} />
    </div>
  )
}
