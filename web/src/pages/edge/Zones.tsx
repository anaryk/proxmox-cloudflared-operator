import { type JSX, useState } from 'react'

import { useApp } from '../../api/store'
import type { ZoneView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { navigate } from '../../app/router'
import { Button } from '../../components/Button'
import { Empty } from '../../components/Empty'
import { Skeleton } from '../../components/Skeleton'
import { type Column, Table } from '../../components/Table'
import { Untrusted } from '../../components/Untrusted'
import { accountName, Card, credentialLabel, unset, useAdminReason } from '../kit'
import { ConfirmLink, CredentialName, leftOutBy, PinDialog, staleEntry, ZoneState } from './ZoneDetail'

export const zonePath = (zone: string) => `/edge/zones/${encodeURIComponent(zone)}`

// Zones is /edge/zones: the zones as the last cycle used them, from the
// engine's own cache rather than the daily checks; a frozen zone with why in
// full, a stale or refused one with the way to let it go, and the zones the
// credentials leave out with what would add them.
export function Zones(): JSX.Element {
  const st = useApp((s) => s.state)
  const refusal = useAdminReason()
  const [pinning, setPinning] = useState<ZoneView>()
  if (!st) return <Skeleton lines={4} label="Loading the state" />
  if (st.zones.length === 0) {
    return (
      <Empty title={unset(st.at) ? 'Waiting for the first cycle.' : 'No zones.'}>
        {unset(st.at) ? null : <p>No credential lists a zone yet: add a Cloudflare API token on the Credentials page.</p>}
      </Empty>
    )
  }

  const routes = new Map<string, number>()
  for (const r of st.routes) if (r.zone) routes.set(r.zone, (routes.get(r.zone) ?? 0) + 1)
  const conflicts = new Map<string, number>()
  for (const c of st.conflicts) conflicts.set(c.zone, (conflicts.get(c.zone) ?? 0) + 1)
  const frozen = st.zones.filter((z) => z.state === 'frozen' || staleEntry(st, z.name))
  const leftOut = st.zones.filter((z) => z.excluded.length > 0)

  const columns: Column<ZoneView>[] = [
    {
      key: 'zone',
      header: 'Zone',
      lead: true,
      cell: (z) => (
        <Link to={zonePath(z.name)}>
          <Untrusted text={z.name} hostname />
        </Link>
      ),
      sort: (a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0),
    },
    { key: 'state', header: 'State', cell: (z) => <ZoneState state={z.state} /> },
    { key: 'account', header: 'Account', cell: (z) => <Untrusted text={accountName(st, z.accountId)} /> },
    { key: 'credentials', header: 'Listed by', cell: (z) => <Untrusted text={z.credentials.map((id) => credentialLabel(st, id)).join(', ') || '-'} /> },
    { key: 'served', header: 'Served by', cell: (z) => (z.servedBy ? <Untrusted text={credentialLabel(st, z.servedBy)} /> : '-') },
    { key: 'pin', header: 'Pin', cell: (z) => (z.pinned ? <Untrusted text={credentialLabel(st, z.pinned)} /> : '-') },
    { key: 'routes', header: 'Routes', className: 'num', cell: (z) => routes.get(z.name) ?? 0 },
    { key: 'conflicts', header: 'Records in the way', className: 'num', cell: (z) => conflicts.get(z.name) ?? 0 },
    {
      key: 'action',
      header: 'Action',
      cell: (z) => (
        <Button small disabledReason={refusal} onClick={() => setPinning(z)}>
          Pin …
        </Button>
      ),
    },
  ]

  return (
    <>
      {frozen.length > 0 && (
        <Card id="zones-frozen" title="Frozen and stale zones">
          <ul className="plain-list">
            {frozen.map((z) => {
              const stale = staleEntry(st, z.name)
              return (
                <li key={z.name} className="zone-why">
                  <p>
                    <Link to={zonePath(z.name)}>
                      <Untrusted text={z.name} hostname />
                    </Link>{' '}
                    <ZoneState state={z.state} />
                  </p>
                  {z.frozenWhy && (
                    <p>
                      <Untrusted text={z.frozenWhy} />
                    </p>
                  )}
                  {stale && (
                    <p>
                      <Untrusted text={stale.detail} /> <ConfirmLink zone={z.name} />
                    </p>
                  )}
                </li>
              )
            })}
          </ul>
        </Card>
      )}
      <Card id="zones-list" title="Zones">
        <Table
          label="Zones"
          columns={columns}
          rows={st.zones}
          rowKey={(z) => z.name}
          defaultSort={{ key: 'zone', direction: 'ascending' }}
          onActivate={(z) => navigate(zonePath(z.name))}
        />
      </Card>
      {leftOut.length > 0 && (
        <Card id="zones-left-out" title="Left out by a credential">
          <ul className="plain-list">
            {leftOut.flatMap((z) =>
              leftOutBy(st, z).map((x) => (
                <li key={`${z.name} ${x.credential}`}>
                  <Untrusted text={z.name} hostname /> is left out by <CredentialName st={st} id={x.credential} />
                  {x.reason && (
                    <>
                      : <Untrusted text={x.reason} />
                    </>
                  )}
                  {x.detail && (
                    <span className="check-detail">
                      <Untrusted text={x.detail} />
                    </span>
                  )}
                </li>
              )),
            )}
          </ul>
        </Card>
      )}
      {pinning && <PinDialog zone={pinning} open onClose={() => setPinning(undefined)} />}
    </>
  )
}
