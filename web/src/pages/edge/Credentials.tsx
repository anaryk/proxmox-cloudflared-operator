import { type JSX, useState } from 'react'

import { useApp } from '../../api/store'
import type { CredentialView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { navigate } from '../../app/router'
import { Button } from '../../components/Button'
import { FailIcon, InfoIcon, OkIcon } from '../../components/icons'
import { Skeleton } from '../../components/Skeleton'
import { StatusBadge } from '../../components/StateBadge'
import { type Column, Table } from '../../components/Table'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { credentialState } from '../../text/words'
import { Card, Failure, useAdminReason, useResource } from '../kit'
import { AddCredential } from './AddCredential'
import { leftOutText, TokenLine } from './Checklist'

// CredentialState is the state of a credential in the words of pco
// credential list: usable, problem for a check that failed, unknown for one
// never checked or whose check got no answer.
export function CredentialState({ view }: { view: CredentialView }) {
  switch (credentialState(view)) {
    case 'usable':
      return (
        <StatusBadge tone="ok" icon={<OkIcon />}>
          usable
        </StatusBadge>
      )
    case 'problem':
      return (
        <StatusBadge tone="fail" icon={<FailIcon />}>
          problem
        </StatusBadge>
      )
  }
  return (
    <StatusBadge tone="idle" icon={<InfoIcon />}>
      unknown
    </StatusBadge>
  )
}

// depthText says how far the last check went.
export const depthText = (v: CredentialView) => (!v.report ? 'never checked' : v.report.deep ? 'write access proven' : 'write access was not tried')

export const credentialPath = (id: string) => `/edge/credentials/${encodeURIComponent(id)}`

// Credentials is /edge/credentials: the tokens pco uses, as the store keeps
// them and their last checks found them, not as the last cycle used them.
export function Credentials(): JSX.Element {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const refusal = useAdminReason()
  const list = useResource<CredentialView[]>('/api/v1/credentials')
  const [adding, setAdding] = useState(false)

  const columns: Column<CredentialView>[] = [
    {
      key: 'label',
      header: 'Label',
      lead: true,
      cell: (v) => (
        <Link to={credentialPath(v.id)}>
          <Untrusted text={v.label || v.id} />
        </Link>
      ),
    },
    { key: 'kind', header: 'Kind', cell: (v) => v.kind || '-' },
    { key: 'state', header: 'State', cell: (v) => <CredentialState view={v} /> },
    { key: 'token', header: 'Token', cell: (v) => (v.report ? <TokenLine report={v.report} /> : '-') },
    { key: 'checked', header: 'Last check', className: 'col-time', cell: (v) => (v.report?.checkedAt ? <Time at={v.report.checkedAt} nodeZone={nodeZone} /> : '-') },
    { key: 'depth', header: 'Depth', cell: depthText },
    { key: 'accounts', header: 'Accounts', cell: (v) => <Untrusted text={(v.report?.accounts ?? []).map((a) => a.name || a.id).join(', ') || '-'} /> },
    { key: 'left', header: 'Zones left out', cell: (v) => <Untrusted text={leftOutText(v.report) || '-'} /> },
  ]

  let body
  if (list.error && !list.data) body = <Failure error={list.error} onTryAgain={list.reload} />
  else if (!list.data) body = <Skeleton lines={3} label="Loading the credentials" />
  else {
    body = (
      <Table
        label="Credentials"
        columns={columns}
        rows={list.data}
        rowKey={(v) => v.id}
        height={360}
        onActivate={(v) => navigate(credentialPath(v.id))}
        empty="No credential yet: add a Cloudflare API token to start."
      />
    )
  }

  return (
    <>
      <Card
        id="credentials-list"
        title="Credentials"
        actions={
          <Button small variant="primary" disabledReason={refusal} aria-expanded={adding} onClick={() => setAdding(!adding)}>
            {adding ? 'Close the form' : 'Add credential'}
          </Button>
        }
      >
        {body}
      </Card>
      {adding && (
        <Card id="credentials-add" title="Add a credential">
          <AddCredential onAdded={() => list.reload()} />
        </Card>
      )}
    </>
  )
}
