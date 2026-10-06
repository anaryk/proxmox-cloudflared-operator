import { useState } from 'react'

import { useApp } from '../../api/store'
import type { ClaimView } from '../../api/types.gen'
import { Button } from '../../components/Button'
import { ConflictIcon, HeldIcon, InfoIcon, OkIcon, WaitIcon } from '../../components/icons'
import { Skeleton } from '../../components/Skeleton'
import { StatusBadge } from '../../components/StateBadge'
import { type Column, Table } from '../../components/Table'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { Card, Failure, Owner, useAdminReason, useResource } from '../kit'
import { ResolveDialog } from './ResolveDialog'

// ClaimState is the state of a claim in its colour, with its icon and word.
export function ClaimState({ state }: { state: string }) {
  switch (state) {
    case 'serving':
      return (
        <StatusBadge tone="ok" icon={<OkIcon />}>
          serving
        </StatusBadge>
      )
    case 'conflict':
      return (
        <StatusBadge tone="fail" icon={<ConflictIcon />}>
          conflict
        </StatusBadge>
      )
    case 'held':
      return (
        <StatusBadge tone="warn" icon={<HeldIcon />}>
          held
        </StatusBadge>
      )
    case 'pending':
      return (
        <StatusBadge tone="info" icon={<WaitIcon />}>
          pending
        </StatusBadge>
      )
  }
  return (
    <StatusBadge tone="idle" icon={<InfoIcon />}>
      <Untrusted text={state || 'unknown'} />
    </StatusBadge>
  )
}

export function Claimants({ claim }: { claim: ClaimView }) {
  if (claim.waiting.length === 0) return <>-</>
  return (
    <>
      {claim.waiting.map((w, at) => (
        <span key={w.owner}>
          {at > 0 && ', '}
          <Owner owner={w.owner} guest={w.guest} />
        </span>
      ))}
    </>
  )
}

// claimColumns are the columns of a list of claims; resolve is the action of
// a claim others wait for.
export function claimColumns(nodeZone: string | undefined, refusal: string | undefined, resolve: (c: ClaimView) => void): Column<ClaimView>[] {
  return [
    { key: 'hostname', header: 'Hostname', lead: true, cell: (c) => <Untrusted text={c.hostname} hostname />, sort: (a, b) => (a.hostname < b.hostname ? -1 : a.hostname > b.hostname ? 1 : 0) },
    { key: 'holder', header: 'Holder', cell: (c) => <Owner owner={c.holder} guest={c.guest} /> },
    { key: 'state', header: 'State', cell: (c) => <ClaimState state={c.state} /> },
    { key: 'since', header: 'Since', className: 'col-time', cell: (c) => <Time at={c.since} nodeZone={nodeZone} /> },
    { key: 'waiting', header: 'Waiting', cell: (c) => <Claimants claim={c} /> },
    {
      key: 'missing',
      header: 'Note',
      cell: (c) =>
        c.missingSince ? (
          <>
            no longer asked for since <Time at={c.missingSince} nodeZone={nodeZone} />
          </>
        ) : null,
    },
    {
      key: 'action',
      header: 'Action',
      cell: (c) =>
        c.waiting.length > 0 ? (
          <Button small disabledReason={refusal} onClick={() => resolve(c)}>
            Hand to …
          </Button>
        ) : null,
    },
  ]
}

// Claims lists who holds each hostname and who waits for it. They are read
// when the page opens, never on every cycle.
export function Claims() {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const refusal = useAdminReason()
  const claims = useResource<ClaimView[]>('/api/v1/claims')
  const [resolving, setResolving] = useState<ClaimView>()

  let body
  if (claims.error && !claims.data) body = <Failure error={claims.error} onTryAgain={claims.reload} />
  else if (!claims.data) body = <Skeleton lines={4} label="Loading the claims" />
  else {
    body = (
      <Table
        label="Claims"
        columns={claimColumns(nodeZone, refusal, setResolving)}
        rows={claims.data}
        rowKey={(c) => c.hostname}
        defaultSort={{ key: 'hostname', direction: 'ascending' }}
        empty="No claims: no guest or manual route names a hostname yet."
      />
    )
  }

  return (
    <Card
      id="guests-claims"
      title="Claims"
      actions={
        <Button small onClick={claims.reload} disabled={claims.loading}>
          Read again
        </Button>
      }
    >
      <p className="muted">
        As the last cycle that settled the claims found it: a claim is serving when its holder publishes the hostname, conflict when others want it too,
        held when nobody serves it, and pending when another owner served it, as after a hand-over. It is unknown until a cycle has settled the claims.
      </p>
      {body}
      <ResolveDialog
        claim={resolving}
        onClose={() => setResolving(undefined)}
        onDone={() => {
          setResolving(undefined)
          claims.reload()
        }}
      />
    </Card>
  )
}
