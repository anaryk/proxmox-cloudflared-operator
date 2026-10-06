import { type JSX, useState } from 'react'

import { useApp } from '../../api/store'
import type { AnnotationView as Annotation, ApprovalView, ClaimView, GuestListView, RouteView } from '../../api/types.gen'
import { EventsTable } from '../../app/EventsTable'
import { Link } from '../../app/Link'
import { Button } from '../../components/Button'
import { Skeleton } from '../../components/Skeleton'
import { StateBadge } from '../../components/StateBadge'
import { type Column, Table } from '../../components/Table'
import { Untrusted } from '../../components/Untrusted'
import { identityNow, routeNote } from '../../text/words'
import { Card, Failure, Owner, useAdminReason, useResource } from '../kit'
import { AnnotationView } from './AnnotationView'
import { type Asked, ApproveDialog, askedFor, useApproveRefusal } from './ApproveDialog'
import { RevokeDialog } from './Approved'
import { Claimants, ClaimState } from './Claims'
import { guestIssues } from './Issues'

const guestRef = /^(qemu|lxc)\/([0-9]{1,9})$/

function Routes({ routes, compact }: { routes: RouteView[]; compact: boolean }) {
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
    { key: 'state', header: 'State', cell: (r) => <StateBadge state={r.state} /> },
    { key: 'service', header: 'Service', cell: (r) => (r.service ? <Untrusted text={r.service} className="mono" /> : '-') },
    { key: 'note', header: 'Note', cell: (r) => <Untrusted text={routeNote(r)} /> },
  ]
  return <Table label="Its routes" columns={columns} rows={routes} rowKey={(r) => r.hostname} height={compact ? 200 : 320} empty="It has no routes." />
}

// GuestDetail is everything about one guest: how the last listing showed it,
// its approval, its routes and claims, the issues of its Notes with the
// route text they are in, and its events. It is the page of the guest and
// the guest's drawer of the map. Any listed guest can be approved here.
export function GuestDetail({ guest, variant }: { guest: string; variant: 'drawer' | 'page' }): JSX.Element {
  const match = guestRef.exec(guest)
  const st = useApp((s) => s.state)
  const digest = useApp((s) => s.state?.digest)
  const refusal = useAdminReason()
  const listed = useResource<GuestListView[]>(match ? '/api/v1/guests' : undefined, digest)
  const approvals = useResource<ApprovalView[]>(match ? '/api/v1/approvals' : undefined, digest)
  const claims = useResource<ClaimView[]>(match ? '/api/v1/claims' : undefined)
  const annotation = useResource<Annotation>(match ? `/api/v1/guests/${match[1]}/${match[2]}/annotation` : undefined, digest)
  const [asked, setAsked] = useState<Asked>()
  const [revoking, setRevoking] = useState<ApprovalView>()
  const compact = variant === 'drawer'

  const me = listed.data?.find((g) => g.ref === guest)
  const approval = approvals.data?.find((a) => a.owner === guest)
  const approveRefusal = useApproveRefusal(askedFor(guest, st, me).identity)

  if (!match) return <p className="form-error"><Untrusted text={guest} /> does not name a guest: a guest is named as qemu/101 or lxc/200.</p>

  const routes = (st?.routes ?? []).filter((r) => r.owner === guest)
  const issues = guestIssues(st?.issues, guest)
  const waits = st?.unapproved.find((g) => `${g.kind}/${g.vmid}` === guest)
  const itsClaims = (claims.data ?? []).filter((c) => c.holder === guest || c.waiting.some((w) => w.owner === guest))
  const name = me?.name ?? waits?.name ?? routes.find((r) => r.guest?.name)?.guest?.name
  // What is approved is what was listed: a guest the listing does not have
  // is not offered, as the daemon would refuse it.
  const unlisted = me || waits ? undefined : listed.data ? 'it is not in the last listing of Proxmox VE' : 'its listing is still loading'

  const lookAgain = () => {
    setAsked(undefined)
    listed.reload()
    approvals.reload()
  }

  return (
    <div className={compact ? 'detail detail-drawer' : 'detail'}>
      <Card
        id={`guest-${match[1]}-${match[2]}`}
        title={<Owner owner={guest} guest={{ name }} />}
        actions={
          <>
            <Button small disabledReason={refusal ?? unlisted ?? approveRefusal} onClick={() => setAsked(askedFor(guest, st, me))}>
              Approve
            </Button>
            {approval && (
              <Button small disabledReason={refusal} onClick={() => setRevoking(approval)}>
                Revoke
              </Button>
            )}
          </>
        }
      >
        {listed.error && !listed.data && <Failure error={listed.error} onTryAgain={listed.reload} />}
        {!listed.data && !listed.error && <Skeleton lines={3} label="Loading the guest" />}
        {listed.data && !me && (
          <p className="muted">{st?.complete === false ? 'The last cycle did not list every guest.' : 'The last listing of Proxmox VE does not have this guest.'}</p>
        )}
        <dl className="details">
          {me && (
            <>
              <dt>Node</dt>
              <dd>
                <Untrusted text={me.node} />
              </dd>
              <dt>Running</dt>
              <dd>{me.running ? 'yes' : 'no'}</dd>
              <dt>Gate tag</dt>
              <dd>{me.tagged ? 'carries it' : 'does not carry it'}</dd>
              <dt>Identity</dt>
              <dd className="mono">{me.identity ? <Untrusted text={me.identity} /> : '-'}</dd>
              <dt>Approval</dt>
              <dd>{me.approval}</dd>
            </>
          )}
          {approval && (
            <>
              <dt>Approved in</dt>
              <dd className="mono">
                <Untrusted text={approval.identity} />
              </dd>
              <dt>Identity now</dt>
              <dd>
                <Untrusted text={identityNow(approval)} />
              </dd>
            </>
          )}
          {waits && (
            <>
              <dt>Waits because</dt>
              <dd>
                <Untrusted text={waits.why.join('; ')} />
              </dd>
              <dt>Would publish</dt>
              <dd>
                {waits.hostnames.map((h, at) => (
                  <span key={h}>
                    {at > 0 && ', '}
                    <Untrusted text={h} hostname />
                  </span>
                ))}
              </dd>
            </>
          )}
        </dl>
      </Card>

      <Card id={`guest-routes-${match[1]}-${match[2]}`} title="Routes">
        <Routes routes={routes} compact={compact} />
      </Card>

      <Card id={`guest-claims-${match[1]}-${match[2]}`} title="Claims">
        {claims.error && !claims.data && <Failure error={claims.error} onTryAgain={claims.reload} />}
        {!claims.data && !claims.error && <Skeleton lines={2} label="Loading the claims" />}
        {claims.data && itsClaims.length === 0 && <p className="muted">It holds no claim and waits for none.</p>}
        {itsClaims.length > 0 && (
          <ul className="plain-list">
            {itsClaims.map((c) => (
              <li key={c.hostname}>
                <Untrusted text={c.hostname} hostname /> <ClaimState state={c.state} />{' '}
                {c.holder === guest ? (
                  c.waiting.length === 0 ? (
                    'it holds it; nobody waits for it'
                  ) : (
                    <>
                      it holds it; waiting: <Claimants claim={c} />
                    </>
                  )
                ) : (
                  <>
                    it waits for it; held by <Owner owner={c.holder} guest={c.guest} />
                  </>
                )}
              </li>
            ))}
          </ul>
        )}
      </Card>

      <Card id={`guest-notes-${match[1]}-${match[2]}`} title={issues.length > 0 ? `Notes: ${issues.length === 1 ? '1 issue' : `${issues.length} issues`}` : 'Notes'}>
        {annotation.error && !annotation.data && <Failure error={annotation.error} onTryAgain={annotation.reload} />}
        {!annotation.data && !annotation.error && <Skeleton lines={3} label="Loading the route text of its Notes" />}
        {annotation.data && <AnnotationView view={annotation.data} />}
      </Card>

      <Card id={`guest-events-${match[1]}-${match[2]}`} title="Events">
        <EventsTable filter={{ guest: [guest] }} live rows={compact ? 5 : 10} />
      </Card>

      <ApproveDialog asked={asked} onClose={() => setAsked(undefined)} onApproved={lookAgain} onLookAgain={lookAgain} />
      <RevokeDialog
        approval={revoking}
        onClose={() => setRevoking(undefined)}
        onDone={() => {
          setRevoking(undefined)
          approvals.reload()
          listed.reload()
        }}
      />
    </div>
  )
}
