import { type JSX, useState } from 'react'

import { useApp } from '../../api/store'
import type { ApprovalView, GuestListView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { navigate } from '../../app/router'
import { Button } from '../../components/Button'
import { Skeleton } from '../../components/Skeleton'
import { type Column, Table } from '../../components/Table'
import { Untrusted } from '../../components/Untrusted'
import { Card, Failure, Owner, type Resource, unset, useResource } from '../kit'
import { type Asked, ApproveDialog, askedFor, useApproveRefusal } from './ApproveDialog'
import { Approved } from './Approved'
import { Claims } from './Claims'
import { Issues } from './Issues'
import { Waiting } from './Waiting'

function ApproveListed({ guest, onApprove }: { guest: GuestListView; onApprove: (a: Asked) => void }) {
  const st = useApp((s) => s.state)
  const asked = askedFor(guest.ref, st, guest)
  const refusal = useApproveRefusal(asked.identity)
  return (
    <Button small disabledReason={refusal} onClick={() => onApprove(asked)}>
      Approve
    </Button>
  )
}

// Tagged lists the guests of the last listing that carry the gate tag; any
// of them can be approved, as pco guest approve takes any listed guest.
function Tagged({ guests, onApprove }: { guests: Resource<GuestListView[]>; onApprove: (a: Asked) => void }) {
  const complete = useApp((s) => s.state?.complete)
  const tagged = (guests.data ?? []).filter((g) => g.tagged)

  const columns: Column<GuestListView>[] = [
    {
      key: 'guest',
      header: 'Guest',
      lead: true,
      cell: (g) => (
        <Link to={`/guests/${g.ref}`}>
          <Owner owner={g.ref} guest={g} />
        </Link>
      ),
    },
    { key: 'node', header: 'Node', cell: (g) => <Untrusted text={g.node} /> },
    { key: 'running', header: 'Running', cell: (g) => (g.running ? 'yes' : 'no') },
    { key: 'identity', header: 'Identity', cell: (g) => (g.identity ? <Untrusted text={g.identity} className="mono" /> : '-') },
    { key: 'approval', header: 'Approval', cell: (g) => g.approval || '-' },
    { key: 'routes', header: 'Routes', className: 'num', cell: (g) => g.routes },
    { key: 'issues', header: 'Issues', className: 'num', cell: (g) => g.issues },
    { key: 'action', header: 'Action', cell: (g) => <ApproveListed guest={g} onApprove={onApprove} /> },
  ]

  let body
  if (guests.error && !guests.data) body = <Failure error={guests.error} onTryAgain={guests.reload} />
  else if (!guests.data) body = <Skeleton lines={4} label="Loading the guests" />
  else {
    body = (
      <Table
        label="All tagged guests"
        columns={columns}
        rows={tagged}
        rowKey={(g) => g.ref}
        onActivate={(g) => navigate(`/guests/${g.ref}`)}
        empty={complete === false ? 'The last cycle did not list every guest.' : 'No guest in the last listing has the gate tag.'}
      />
    )
  }
  return (
    <Card id="guests-tagged" title="All tagged guests">
      {body}
    </Card>
  )
}

// GuestsPage is /guests: who waits for approval, who is approved, what the
// Notes get wrong and every tagged guest; and /guests/claims, who holds each
// hostname. The lists of the daemon are read when the page opens, those of
// guests and approvals again when the state changes.
export function GuestsPage({ claims }: { claims?: boolean }): JSX.Element {
  const at = useApp((s) => s.state?.at)
  const loaded = useApp((s) => s.state !== undefined)
  const digest = useApp((s) => s.state?.digest)
  const guests = useResource<GuestListView[]>(claims ? undefined : '/api/v1/guests', digest)
  const approvals = useResource<ApprovalView[]>(claims ? undefined : '/api/v1/approvals', digest)
  const [asked, setAsked] = useState<Asked>()

  const again = () => {
    setAsked(undefined)
    guests.reload()
    approvals.reload()
  }

  return (
    <>
      <nav className="subnav" aria-label="Guests">
        <Link to="/guests" aria-current={claims ? undefined : 'page'}>
          Guests
        </Link>
        <Link to="/guests/claims" aria-current={claims ? 'page' : undefined}>
          Claims
        </Link>
      </nav>
      {claims ? (
        <Claims />
      ) : (
        <>
          {!loaded && <Skeleton lines={4} label="Loading the state" />}
          {loaded && unset(at) && <p className="muted">Waiting for the first cycle.</p>}
          <Waiting onApprove={setAsked} />
          <Approved approvals={approvals} />
          <Issues />
          <Tagged guests={guests} onApprove={setAsked} />
          <ApproveDialog asked={asked} onClose={() => setAsked(undefined)} onApproved={again} onLookAgain={again} />
        </>
      )}
    </>
  )
}
