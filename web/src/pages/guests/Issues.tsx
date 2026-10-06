import { useApp } from '../../api/store'
import type { Issue } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { type Column, Table } from '../../components/Table'
import { Untrusted } from '../../components/Untrusted'
import { waitingApprovalIssue } from '../../gen/words.gen'
import { Card } from '../kit'

export type GuestIssue = Issue & { guest: NonNullable<Issue['guest']> }

// guestIssues are the issues of the Notes of guests: those of the settings
// have no guest and are shown with the settings, and the one the engine adds
// for every guest that waits for approval is no fault of its Notes: that
// guest is in the list of those that wait already.
export function guestIssues(issues: readonly Issue[] | undefined, owner?: string): GuestIssue[] {
  return (issues ?? []).filter(
    (is): is is GuestIssue => is.guest !== undefined && is.msg !== waitingApprovalIssue && (owner === undefined || `${is.guest.kind}/${is.guest.vmid}` === owner),
  )
}

const refOf = (is: GuestIssue) => `${is.guest.kind}/${is.guest.vmid}`

// Issues lists what the last cycle found wrong in the Notes of the guests.
export function Issues() {
  const issues = guestIssues(useApp((s) => s.state?.issues))

  const columns: Column<GuestIssue>[] = [
    {
      key: 'guest',
      header: 'Guest',
      lead: true,
      cell: (is) => <Link to={`/guests/${is.guest.kind}/${is.guest.vmid}`}>{refOf(is)}</Link>,
    },
    { key: 'line', header: 'Line', className: 'num', cell: (is) => (is.line ? is.line : '-') },
    { key: 'col', header: 'Column', className: 'num', cell: (is) => (is.col ? is.col : '-') },
    { key: 'msg', header: 'Message', cell: (is) => <Untrusted text={is.msg} /> },
  ]

  return (
    <Card id="guests-issues" title="Annotation issues">
      <Table
        label="Annotation issues"
        columns={columns}
        rows={issues}
        rowKey={(is) => `${refOf(is)}:${is.line ?? 0}:${is.col ?? 0}:${is.msg}`}
        height={320}
        empty="No issues in guest notes."
      />
    </Card>
  )
}
