import { useState } from 'react'

import { api, type ApiError } from '../../api/client'
import type { ApprovalView } from '../../api/types.gen'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { Dialog } from '../../components/Dialog'
import { Skeleton } from '../../components/Skeleton'
import { type Column, Table } from '../../components/Table'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { identityNow } from '../../text/words'
import { asApiError, Card, Failure, Owner, ownerText, type Resource, useAdminReason } from '../kit'

// RevokeDialog asks before an approval is removed, and says what that does.
// onDone closes it and reads the approvals again.
export function RevokeDialog({ approval, onClose, onDone }: { approval?: ApprovalView; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<ApiError>()

  const close = () => {
    setError(undefined)
    onClose()
  }

  const revoke = async () => {
    if (!approval) return
    setSending(true)
    setError(undefined)
    try {
      await api('POST', '/api/v1/guests/revoke', { owner: approval.owner })
      toast(<Untrusted text={`Revoked the approval of ${ownerText(approval.owner, approval.guest)}.`} />, 'ok')
      onDone()
    } catch (e) {
      setError(asApiError(e))
    } finally {
      setSending(false)
    }
  }

  return (
    <Dialog
      open={approval !== undefined}
      onClose={close}
      title="Revoke an approval"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button variant="danger" disabled={sending} onClick={() => void revoke()}>
            Revoke
          </Button>
        </>
      }
    >
      {approval && (
        <>
          <p>
            <Owner owner={approval.owner} guest={approval.guest} /> is approved in identity{' '}
            <b className="mono">
              <Untrusted text={approval.identity} />
            </b>
            .
          </p>
          <p>Revoking it stops its routes from being published while the admission mode is approve.</p>
          {sending && <Busy label="Revoking" />}
          {error && (
            <Failure
              error={error}
              onLookAgain={() => {
                setError(undefined)
                onDone()
              }}
              onTryAgain={() => void revoke()}
            />
          )}
        </>
      )}
    </Dialog>
  )
}

// Approved lists the approvals with the identity each was made in and
// whether the guest has it still.
export function Approved({ approvals }: { approvals: Resource<ApprovalView[]> }) {
  const refusal = useAdminReason()
  const [revoking, setRevoking] = useState<ApprovalView>()

  const columns: Column<ApprovalView>[] = [
    { key: 'guest', header: 'Guest', lead: true, cell: (a) => <Owner owner={a.owner} guest={a.guest} /> },
    { key: 'identity', header: 'Identity approved', cell: (a) => <Untrusted text={a.identity} className="mono" /> },
    { key: 'now', header: 'Identity now', cell: (a) => <Untrusted text={identityNow(a)} /> },
    {
      key: 'action',
      header: 'Action',
      cell: (a) => (
        <Button small disabledReason={refusal} onClick={() => setRevoking(a)}>
          Revoke
        </Button>
      ),
    },
  ]

  let body
  if (approvals.error && !approvals.data) body = <Failure error={approvals.error} onTryAgain={approvals.reload} />
  else if (!approvals.data) body = <Skeleton lines={2} label="Loading the approvals" />
  else body = <Table label="Approved guests" columns={columns} rows={approvals.data} rowKey={(a) => a.owner} height={320} empty="No guest is approved." />

  return (
    <Card id="guests-approved" title="Approved">
      {body}
      <RevokeDialog
        approval={revoking}
        onClose={() => setRevoking(undefined)}
        onDone={() => {
          setRevoking(undefined)
          approvals.reload()
        }}
      />
    </Card>
  )
}
