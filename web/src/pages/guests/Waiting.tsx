import { useApp } from '../../api/store'
import type { UnapprovedGuest } from '../../api/types.gen'
import { Button } from '../../components/Button'
import { type Column, Table } from '../../components/Table'
import { Untrusted } from '../../components/Untrusted'
import { Card, Owner } from '../kit'
import { type Asked, askedFor, useApproveRefusal } from './ApproveDialog'

const refOf = (g: UnapprovedGuest) => `${g.kind}/${g.vmid}`

function ApproveButton({ guest, onApprove }: { guest: UnapprovedGuest; onApprove: () => void }) {
  const refusal = useApproveRefusal(guest.identity)
  return (
    <Button small disabledReason={refusal} onClick={onApprove}>
      Approve
    </Button>
  )
}

// Waiting lists the guests that wait for approval, with the identity an
// approval would be of and the hostnames it would publish.
export function Waiting({ onApprove }: { onApprove: (a: Asked) => void }) {
  const st = useApp((s) => s.state)
  const waiting = st?.unapproved ?? []

  const columns: Column<UnapprovedGuest>[] = [
    { key: 'guest', header: 'Guest', lead: true, cell: (g) => <Owner owner={refOf(g)} guest={g} /> },
    { key: 'identity', header: 'Identity', cell: (g) => (g.identity ? <Untrusted text={g.identity} className="mono" /> : '-') },
    {
      key: 'hostnames',
      header: 'Would publish',
      cell: (g) =>
        g.hostnames.map((h, at) => (
          <span key={h}>
            {at > 0 && ', '}
            <Untrusted text={h} hostname />
          </span>
        )),
    },
    { key: 'why', header: 'Why it waits', cell: (g) => <Untrusted text={g.why.join('; ')} /> },
    {
      key: 'action',
      header: 'Action',
      cell: (g) => <ApproveButton guest={g} onApprove={() => onApprove(askedFor(refOf(g), st))} />,
    },
  ]

  return (
    <Card id="guests-waiting" title={`Waiting for approval${waiting.length > 0 ? ` (${waiting.length})` : ''}`}>
      <Table
        label="Guests waiting for approval"
        columns={columns}
        rows={waiting}
        rowKey={refOf}
        height={320}
        empty="No guest waits for approval."
      />
    </Card>
  )
}
