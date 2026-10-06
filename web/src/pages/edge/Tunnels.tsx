import type { JSX } from 'react'

import { useApp } from '../../api/store'
import type { TunnelView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { navigate } from '../../app/router'
import { Empty } from '../../components/Empty'
import { Skeleton } from '../../components/Skeleton'
import { type Column, Table } from '../../components/Table'
import { Untrusted } from '../../components/Untrusted'
import { Card, planEntry, unset } from '../kit'
import { connectorLine, connectorOf, rolloutText, tunnelLabel, tunnelPath, verifiedLine } from './TunnelDetail'

// Tunnels is /edge/tunnels: the tunnel of the install in each account, told
// apart by the account, with what the last cycle found of each and the
// connectors pco does not run; and the tunnels no credential sees any more,
// which wait for the shared confirmation.
export function Tunnels(): JSX.Element {
  const st = useApp((s) => s.state)
  if (!st) return <Skeleton lines={4} label="Loading the state" />
  const unseen = st.waiting.filter((w) => w.kind === 'unseen-tunnel')
  const rogues = new Map<string, number>()
  for (const r of st.rogueConnectors) rogues.set(r.accountId, (rogues.get(r.accountId) ?? 0) + 1)

  const columns: Column<TunnelView>[] = [
    {
      key: 'tunnel',
      header: 'Tunnel',
      lead: true,
      cell: (t) => (
        <Link to={tunnelPath(t.accountId)}>
          <Untrusted text={tunnelLabel(st, t)} />
        </Link>
      ),
    },
    { key: 'name', header: 'Name', cell: (t) => <Untrusted text={t.name} className="mono" /> },
    { key: 'verified', header: 'Verified', cell: (t) => <Untrusted text={verifiedLine(t, st.hold)} /> },
    { key: 'rollout', header: 'Rollout', cell: (t) => rolloutText(t.rollout) },
    { key: 'connector', header: 'Connector', cell: (t) => connectorLine(connectorOf(st, t)) },
    {
      key: 'rogue',
      header: 'Not run by pco',
      className: 'num',
      cell: (t) => {
        const n = rogues.get(t.accountId) ?? 0
        return n > 0 ? <b className="rogue-count">{n}</b> : 0
      },
    },
  ]

  return (
    <>
      {unseen.length > 0 && (
        <Card id="tunnels-unseen" title="Tunnels no credential sees">
          <ul className="plain-list">
            {unseen.map((w) => (
              <li key={`${w.subject} ${w.detail}`}>
                <Untrusted text={w.detail} /> <Link to={planEntry(w.kind, w.subject)}>Confirm in the plan</Link>
              </li>
            ))}
          </ul>
        </Card>
      )}
      <Card id="tunnels-list" title="Tunnels">
        {st.tunnels.length === 0 ? (
          <Empty title={unset(st.at) ? 'Waiting for the first cycle.' : 'No tunnels.'}>
            {unset(st.at) ? null : <p>No tunnel of this install is known yet.</p>}
          </Empty>
        ) : (
          <Table label="Tunnels" columns={columns} rows={st.tunnels} rowKey={(t) => t.accountId} height={360} onActivate={(t) => navigate(tunnelPath(t.accountId))} />
        )}
      </Card>
    </>
  )
}
