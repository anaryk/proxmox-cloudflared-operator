import './nodes.css'

import { type JSX, useState, useSyncExternalStore } from 'react'

import { useApp } from '../../api/store'
import type { BlockView, ClusterView, LeaseView, NodeView, State } from '../../api/types.gen'
import { Head } from '../../app/Head'
import { Link } from '../../app/Link'
import { navigate, type View } from '../../app/router'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Drawer } from '../../components/Drawer'
import { Empty } from '../../components/Empty'
import { Skeleton } from '../../components/Skeleton'
import { StatusBadge } from '../../components/StateBadge'
import { type Column, Table } from '../../components/Table'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { durationText, nanosToMs } from '../../text/duration'
import { nodeAgentText, nodeRoleText, rotationText } from '../../text/words'
import { Card, useAdminReason } from '../kit'
import { HandoffDialog, JoinTokenDialog, UnblockDialog } from './ClusterDialogs'
import { NodeDetail, NodeStateBadge } from './NodeDetail'
import { clustered, compareNodes, kindText, nodeLink, notController, skewText, targetsText } from './nodes'

const phone = '(max-width: 719px)'

function followPhone(changed: () => void): () => void {
  const query = window.matchMedia(phone)
  query.addEventListener('change', changed)
  return () => query.removeEventListener('change', changed)
}

function usePhone(): boolean {
  return useSyncExternalStore(followPhone, () => window.matchMedia(phone).matches)
}

const list = (names: readonly string[] | undefined) => (names && names.length > 0 ? names.join(', ') : undefined)

function Lease({ lease, nodeZone }: { lease: LeaseView; nodeZone?: string }) {
  const state = lease.behind ? 'behind' : lease.fenced ? 'fenced' : lease.valid ? 'valid' : 'not valid'
  const tone = lease.behind || lease.fenced ? 'fail' : lease.valid ? 'ok' : 'warn'
  return (
    <>
      <dt>Controller</dt>
      <dd>
        {lease.holder ? (
          <>
            <Untrusted className="mono" text={lease.holder} />
            {lease.address && (
              <>
                {' '}
                at <Untrusted className="mono" text={lease.address} />
              </>
            )}
            , generation <span className="num">{lease.generation}</span>
            {lease.acquiredAt && (
              <>
                , since <Time at={lease.acquiredAt} nodeZone={nodeZone} />
              </>
            )}
          </>
        ) : (
          <span className="muted">no controller is known</span>
        )}
      </dd>
      <dt>Lease</dt>
      <dd>
        <StatusBadge tone={tone}>{state}</StatusBadge>
        {lease.valid && (
          <>
            {' '}
            <span className="muted">refreshed {durationText(nanosToMs(lease.refreshAge))} ago</span>
          </>
        )}
        {lease.fenced && (
          <p>
            Fenced: <Untrusted text={lease.fenced} />
          </p>
        )}
        {lease.behind && (
          <p>
            Behind: <Untrusted text={lease.behind} />
          </p>
        )}
        {lease.handoff && (
          <p>
            Being handed off to <Untrusted className="mono" text={lease.handoff} />.
          </p>
        )}
        {lease.previous && (
          <p>
            Held by <Untrusted className="mono" text={lease.previous} /> before.
          </p>
        )}
      </dd>
    </>
  )
}

function Cluster({ cluster, nodeZone }: { cluster: ClusterView; nodeZone?: string }) {
  const c = cluster.coverage
  const rotation = rotationText(cluster.caRotation)
  return (
    <>
      <dt>Members</dt>
      <dd>
        <Untrusted text={list(cluster.members) ?? 'none listed'} />
      </dd>
      <dt>Quorum</dt>
      <dd>
        <StatusBadge tone={cluster.quorate ? 'ok' : 'fail'}>{cluster.quorate ? 'quorate' : 'not quorate'}</StatusBadge>
        {cluster.quorumDevice && ' with a quorum device'}
        {list(cluster.overrides) && (
          <>
            ; overrides: <Untrusted className="mono" text={list(cluster.overrides) ?? ''} />, under which pco relies on its sentinel alone
          </>
        )}
      </dd>
      <dt>Coverage</dt>
      <dd>
        <StatusBadge tone={c.covered ? 'ok' : 'warn'}>{c.covered ? 'every node watches' : 'lost'}</StatusBadge>
        {c.since && (
          <>
            {' '}
            since <Time at={c.since} nodeZone={nodeZone} />
          </>
        )}
        {list(c.uncovered) && (
          <p>
            Without a running watch: <Untrusted className="mono" text={list(c.uncovered) ?? ''} />; routes of other nodes&apos; guests are pinned to their nodes.
          </p>
        )}
        {list(c.flapping) && (
          <p>
            Broke twice within ten minutes: <Untrusted className="mono" text={list(c.flapping) ?? ''} />; routes stay pinned until the watch runs for ten minutes.
          </p>
        )}
        {c.promoteAt && c.covered && (
          <p>
            The routes it demoted are shared again at <Time at={c.promoteAt} nodeZone={nodeZone} />.
          </p>
        )}
      </dd>
      {cluster.upgrade && (
        <>
          <dt>Upgrade</dt>
          <dd>
            From release <Untrusted text={cluster.upgrade.from} />, unfinished
            {cluster.upgrade.since && (
              <>
                {' '}
                since <Time at={cluster.upgrade.since} nodeZone={nodeZone} />
              </>
            )}
            ; <span className="mono">pco cluster finish-upgrade</span> on the controller ends it once every node runs this release
            {list(cluster.upgrade.older) && (
              <>
                {' '}
                (still older: <Untrusted className="mono" text={list(cluster.upgrade.older) ?? ''} />)
              </>
            )}
            .
          </dd>
        </>
      )}
      {rotation && (
        <>
          <dt>Rotation</dt>
          <dd>
            <Untrusted text={rotation} />
            {list(cluster.caRotation?.refused) && (
              <p>
                Refused by the new CA: <Untrusted className="mono" text={list(cluster.caRotation?.refused) ?? ''} />; each issues its certificate again at its next start of pco.
              </p>
            )}
          </dd>
        </>
      )}
    </>
  )
}

function Blocks({ blocks, onUnblock }: { blocks: readonly BlockView[]; onUnblock: (b: BlockView) => void }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const refusal = useAdminReason()
  const columns: Column<BlockView>[] = [
    { key: 'address', header: 'Address', lead: true, cell: (b) => <Untrusted className="mono" text={b.address} /> },
    { key: 'guest', header: 'Guest', cell: (b) => <Untrusted className="mono" text={b.guest} /> },
    {
      key: 'seen',
      header: 'Seen on',
      cell: (b) => (
        <>
          <Untrusted className="mono" text={b.port} /> of <Untrusted className="mono" text={b.node} />
          {b.mac && (
            <>
              {' '}
              (MAC <Untrusted className="mono" text={b.mac} />)
            </>
          )}
        </>
      ),
    },
    { key: 'since', header: 'Since', cell: (b) => (b.since ? <Time at={b.since} nodeZone={nodeZone} /> : '-') },
    { key: 'last', header: 'Last report', cell: (b) => (b.lastReport ? <Time at={b.lastReport} nodeZone={nodeZone} /> : '-') },
    {
      key: 'action',
      header: 'Action',
      cell: (b) => (
        <Button small disabledReason={refusal} onClick={() => onUnblock(b)}>
          End the block…
        </Button>
      ),
    },
  ]
  return <Table label="Blocked addresses" columns={columns} rows={blocks} rowKey={(b) => b.address} height={240} />
}

// NodesTable is a row for each node of the registry, in the order of pco
// node list; a row opens the node.
function NodesTable({ nodes, current, onOpen }: { nodes: readonly NodeView[]; current?: string; onOpen: (n: NodeView) => void }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const columns: Column<NodeView>[] = [
    {
      key: 'name',
      header: 'Node',
      lead: true,
      cell: (n) => (
        <>
          <span className="cell-line">
            <Link to={nodeLink(n.name)} className="mono">
              <Untrusted text={n.name} />
            </Link>{' '}
            {n.controller && <Badge tone="info">controller</Badge>}
          </span>
          <span className="cell-line muted">
            {kindText(n)}
            {n.build && (
              <>
                {' '}
                · <Untrusted text={n.build} />
              </>
            )}
          </span>
        </>
      ),
      sort: compareNodes,
    },
    {
      key: 'state',
      header: 'State',
      cell: (n) => (
        <>
          <NodeStateBadge state={n.state} />
          {n.drained && (
            <>
              {' '}
              <Badge tone="info">drained</Badge>
            </>
          )}
        </>
      ),
      sort: compareNodes,
    },
    { key: 'role', header: 'Role', cell: (n) => nodeRoleText(n) },
    { key: 'agent', header: 'Agent', cell: (n) => <Untrusted text={nodeAgentText(n)} /> },
    {
      key: 'replica',
      header: 'Replica',
      cell: (n) => (n.state === 'removed' ? '-' : <StatusBadge tone={n.replicaReady ? 'ok' : 'idle'}>{n.replicaReady ? 'ready' : 'not ready'}</StatusBadge>),
    },
    { key: 'targets', header: 'Targets', cell: (n) => targetsText(n) },
    { key: 'cert', header: 'Certificate', cell: (n) => (n.certExpires ? <Time at={n.certExpires} nodeZone={nodeZone} /> : '-') },
    { key: 'skew', header: 'Clock skew', className: 'num', cell: (n) => skewText(n.clockSkew) },
  ]
  return (
    <Table label="Nodes" columns={columns} rows={nodes} rowKey={(n) => n.name} defaultSort={{ key: 'state', direction: 'ascending' }} current={current} onActivate={onOpen} lines={2} />
  )
}

// NodesPage is /nodes and /nodes/:name: the cluster as the controller's
// last cycle found it, the lease, a row for each node of the registry, the
// addresses blocked on every node, and the detail of the node selected,
// whose address is the page's.
export function NodesPage({ view }: { view: Extract<View, { name: 'nodes' | 'node' }> }): JSX.Element {
  const st = useApp((s) => s.state)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const refusal = useAdminReason()
  const onPhone = usePhone()
  const [joining, setJoining] = useState(false)
  const [handing, setHanding] = useState(false)
  const [unblocking, setUnblocking] = useState<BlockView>()

  const selected = view.name === 'node' ? view.node : undefined
  const head = (
    <Head
      title="Nodes"
      description="The nodes of the install, the controller that writes for them, and what each agent last reported."
      actions={
        <>
          <Button disabledReason={refusal ?? (st && !clustered(st) ? 'a node alone has nothing to join it' : undefined)} onClick={() => setJoining(true)}>
            Join token…
          </Button>
          <Button disabledReason={refusal ?? (st && !st.lease ? 'the appliance has no lease to hand off' : undefined)} onClick={() => setHanding(true)}>
            Hand off the lease…
          </Button>
        </>
      }
    />
  )

  if (selected && onPhone) {
    return (
      <>
        <p>
          <Link to="/nodes">All nodes</Link>
        </p>
        <NodeDetail key={selected} name={selected} variant="page" />
      </>
    )
  }

  const drawer = (
    <Drawer open={selected !== undefined} onClose={() => navigate('/nodes')} title={selected ? <Untrusted text={selected} /> : ''}>
      {selected && <NodeDetail key={selected} name={selected} variant="drawer" />}
    </Drawer>
  )

  return (
    <>
      {head}
      {st ? <Body st={st} nodeZone={nodeZone} selected={selected} onUnblock={setUnblocking} /> : <Skeleton lines={6} label="Loading the nodes" />}
      {drawer}
      <JoinTokenDialog open={joining} onClose={() => setJoining(false)} />
      <HandoffDialog open={handing} nodes={st?.nodes ?? []} controller={st?.lease?.holder || st?.cluster?.controller || st?.node || ''} onClose={() => setHanding(false)} />
      <UnblockDialog block={unblocking} onClose={() => setUnblocking(undefined)} />
    </>
  )
}

function Body({ st, nodeZone, selected, onUnblock }: { st: State; nodeZone?: string; selected?: string; onUnblock: (b: BlockView) => void }) {
  const nodes = [...st.nodes].sort(compareNodes)
  const blocks = st.cluster?.blocks ?? []
  return (
    <>
      {notController(st) && (
        <Card id="nodes-elsewhere" title="This node is not the controller">
          <p>
            {st.lease?.holder ? (
              <>
                Node <Untrusted className="mono" text={st.lease.holder} /> holds the lease: it runs the cycles and serves the pages of the cluster. This page shows what this
                node&apos;s agent knows; every action is refused here.
              </>
            ) : (
              <>No controller is known: no node holds the lease now. This page shows what this node&apos;s agent knows; every action is refused here.</>
            )}{' '}
            <Link to="/agent">This node&apos;s agent</Link>
          </p>
        </Card>
      )}
      {(st.lease || st.cluster) && (
        <Card id="nodes-cluster" title="Cluster">
          <dl className="details">
            {st.lease && <Lease lease={st.lease} nodeZone={nodeZone} />}
            {!st.lease && st.cluster && (
              <>
                <dt>Controller</dt>
                <dd>
                  <Untrusted className="mono" text={st.cluster.controller} />, generation <span className="num">{st.cluster.generation}</span>
                </dd>
              </>
            )}
            {st.cluster && <Cluster cluster={st.cluster} nodeZone={nodeZone} />}
          </dl>
        </Card>
      )}
      <Card id="nodes-list" title="Nodes">
        {nodes.length === 0 ? (
          <Empty title={clustered(st) ? 'No node is registered.' : 'This node stands alone.'}>
            <p>
              {clustered(st)
                ? 'The registry of the install lists no node yet.'
                : 'No other node is registered. pco setup on another node of the Proxmox cluster joins it, and the pages of the cluster appear here.'}
            </p>
          </Empty>
        ) : (
          <NodesTable nodes={nodes} current={selected} onOpen={(n) => navigate(nodeLink(n.name))} />
        )}
      </Card>
      {blocks.length > 0 && (
        <Card id="nodes-blocks" title="Blocked addresses">
          <p className="muted">An address every node blocks because an agent saw the MAC its guest is bound to on the port of another guest. A block ends by itself once the conflict is gone.</p>
          <Blocks blocks={blocks} onUnblock={onUnblock} />
        </Card>
      )}
    </>
  )
}
