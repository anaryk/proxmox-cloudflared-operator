import { type JSX, useState } from 'react'

import { useApp } from '../../api/store'
import type { NodeView, RouteView, State } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Empty } from '../../components/Empty'
import { LevelBadge, StatusBadge } from '../../components/StateBadge'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { nodeAgentText, nodeRoleText, nodeStateText } from '../../text/words'
import { Card, useAdminReason } from '../kit'
import { routeLink } from '../routes/routes'
import { AdmitDialog, JoinUses } from './AdmitDialog'
import { type DrainAsk, DrainDialog } from './DrainDialog'
import { ForgetDialog } from './ForgetDialog'
import { kindText, nodeTone, replicaLevelText, replicaTone, skewText, targetsText } from './nodes'

// routesOn are the routes a node has a part in: those pinned to it, and
// those it is a replica of.
export function routesOn(st: Pick<State, 'routes'>, name: string): { pinned: RouteView[]; replica: RouteView[] } {
  const pinned = st.routes.filter((r) => r.placement?.kind === 'pinned' && r.placement.node === name)
  const replica = st.routes.filter((r) => r.placement?.kind !== 'pinned' && r.replicas.some((x) => x.node === name))
  return { pinned, replica }
}

// NodeStateBadge is a node's state in its colour, with the word.
export function NodeStateBadge({ state }: { state: string }) {
  return (
    <StatusBadge tone={nodeTone(state)}>
      <Untrusted text={state || 'not numbered'} />
    </StatusBadge>
  )
}

function Routes({ st, name, routes, label }: { st: State; name: string; routes: readonly RouteView[]; label: string }) {
  if (routes.length === 0) return null
  return (
    <>
      <h3>{label}</h3>
      <ul className="replica-list" aria-label={label}>
        {routes.map((r) => {
          const mine = r.replicas.find((x) => x.node === name)
          return (
            <li key={`${r.hostname}\u0000${r.owner}`}>
              <Link to={routeLink(r.hostname, r.owner)} className="mono">
                <Untrusted text={r.hostname} hostname />
              </Link>
              {mine && (
                <>
                  <StatusBadge tone={replicaTone(mine)}>{mine.ok ? (mine.inSet ? 'reached, in its set' : 'reached, not in its set yet') : 'not reached'}</StatusBadge>
                  <Badge outline>
                    <Untrusted text={replicaLevelText(mine)} />
                  </Badge>
                  {mine.reason && (
                    <span className="muted">
                      <Untrusted text={mine.reason} />
                    </span>
                  )}
                </>
              )}
              {!mine && r.placement?.why && (
                <span className="muted">
                  <Untrusted text={r.placement.why} />
                </span>
              )}
            </li>
          )
        })}
      </ul>
      {st.routes.length === 0 && null}
    </>
  )
}

// NodeEvents lists what the stream brought of a node since the page opened:
// the events that name it. The events page has the rest.
function NodeEvents({ name }: { name: string }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const live = useApp((s) => s.events)
  const events = live.filter((e) => e.node === name).sort((a, b) => b.seq - a.seq)
  if (events.length === 0) return <p className="muted">No event of this node reached the page yet.</p>
  return (
    <ol className="timeline" aria-label="Events of the node">
      {events.slice(0, 20).map((e) => (
        <li key={`${e.boot ?? ''}:${e.seq}`} className={e.level === 'error' ? 'timeline-entry timeline-error' : 'timeline-entry'}>
          <div className="timeline-head">
            <Time at={e.at ?? ''} nodeZone={nodeZone} /> <LevelBadge level={e.level} />{' '}
            <span className="muted">
              <Untrusted text={e.kind} />
            </span>
            {e.actor && (
              <span className="timeline-actor">
                by <Untrusted text={e.actor} />
              </span>
            )}
          </div>
          <div className="timeline-message">
            <Untrusted text={e.message} />
          </div>
        </li>
      ))}
    </ol>
  )
}

// NodeDetail is one node of the registry: its state and role, what its
// agent last reported, the uses of its join token, the routes it has a
// part in, its events, and what an admin does to it. In a drawer the
// drawer names the node; as a page it is the heading of the page.
export function NodeDetail({ name, variant }: { name: string; variant: 'drawer' | 'page' }): JSX.Element {
  const st = useApp((s) => s.state)
  const rotateOnForget = useApp((s) => s.settings?.settings.cluster?.rotateOnForget ?? true)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const refusal = useAdminReason()
  const [admitting, setAdmitting] = useState<NodeView>()
  const [draining, setDraining] = useState<DrainAsk>()
  const [forgetting, setForgetting] = useState<NodeView>()

  const title = variant === 'page' && (
    <h1 className="detail-title mono">
      <Untrusted text={name} />
    </h1>
  )
  const node = st?.nodes.find((n) => n.name === name)
  if (!st || !node) {
    return (
      <div className={`detail detail-${variant}`}>
        {title}
        <Empty title="No node of this name is in the registry">
          <p>
            <Link to="/nodes">All nodes</Link>
          </p>
        </Empty>
      </div>
    )
  }

  const on = routesOn(st, name)
  const removed = node.state === 'removed'
  const refusedWhy = (why: string | undefined) => refusal ?? why
  return (
    <div className={`detail detail-${variant}`}>
      {title}
      <p className="node-summary">
        <NodeStateBadge state={node.state} />
        <span className="muted">
          <Untrusted text={nodeStateText(node.state)} />
        </span>
        <Badge outline>{nodeRoleText(node)}</Badge>
        <Badge outline>{kindText(node)}</Badge>
        {node.drained && <Badge tone="info">drained ({node.drained})</Badge>}
      </p>
      <div className="node-actions">
        {node.state === 'joining' && (
          <Button small variant="primary" disabledReason={refusedWhy(undefined)} onClick={() => setAdmitting(node)}>
            Admit…
          </Button>
        )}
        {!removed && !node.drained && (
          <Button small disabledReason={refusedWhy(node.state === 'contested' ? 'a contested node serves nothing to drain' : undefined)} onClick={() => setDraining({ node, undrain: false })}>
            Drain…
          </Button>
        )}
        {!removed && node.drained && (
          <Button small disabledReason={refusedWhy(undefined)} onClick={() => setDraining({ node, undrain: true })}>
            End the drain…
          </Button>
        )}
        {!removed && (
          <Button small variant="danger" disabledReason={refusedWhy(node.controller ? 'the controller is taken out of the install with pco uninstall on it' : undefined)} onClick={() => setForgetting(node)}>
            Forget…
          </Button>
        )}
      </div>
      <dl className="details">
        <dt>Agent</dt>
        <dd>
          <Untrusted text={nodeAgentText(node)} />
          {node.build && (
            <>
              {' '}
              · build <Untrusted text={node.build} />
              {node.protocol && (
                <>
                  , protocol <Untrusted text={node.protocol} />
                </>
              )}
            </>
          )}
        </dd>
        <dt>Since</dt>
        <dd>{node.stateSince ? <Time at={node.stateSince} nodeZone={nodeZone} /> : <span className="muted">-</span>}</dd>
        {node.agentAddress && (
          <>
            <dt>Agent address</dt>
            <dd className="mono">
              <Untrusted text={node.agentAddress} />
            </dd>
          </>
        )}
        <dt>Index</dt>
        <dd className="num">{node.index === undefined ? <span className="muted">none before routing admission</span> : node.index}</dd>
        <dt>Coverage</dt>
        <dd>
          {node.watchRunning ? 'its watch runs' : 'no running watch'}; {node.covered ? "another node's proof of its ports counts" : 'routes of its guests are pinned to it'}
        </dd>
        <dt>Replica</dt>
        <dd>{node.replicaReady ? 'ready: its shared connector runs' : 'not ready'}</dd>
        <dt>Targets</dt>
        <dd>{targetsText(node)}</dd>
        <dt>Certificate</dt>
        <dd>
          {node.certExpires ? (
            <>
              expires <Time at={node.certExpires} nodeZone={nodeZone} />
            </>
          ) : (
            <span className="muted">no certificate enrolled</span>
          )}
        </dd>
        <dt>Clock skew</dt>
        <dd className="num">{skewText(node.clockSkew)}</dd>
      </dl>
      {(node.joinUses ?? []).length > 0 && (
        <>
          <h3>Uses of its join token</h3>
          <JoinUses uses={node.joinUses} label="Uses of the join token" />
        </>
      )}
      {(node.contested ?? []).length > 0 && (
        <>
          <h3>The uses that contested it</h3>
          <JoinUses uses={node.contested} label="Contested uses of the join token" />
          <p className="muted">Its join token was used more than once, or from where it should not: only forgetting the node ends that, and a new token enrols it again.</p>
        </>
      )}
      <Routes st={st} name={name} routes={on.pinned} label="Routes pinned to it" />
      <Routes st={st} name={name} routes={on.replica} label="Routes it is a replica of" />
      <Card id={`node-events-${variant}`} title="Events">
        <NodeEvents name={name} />
      </Card>
      <AdmitDialog node={admitting} onClose={() => setAdmitting(undefined)} onAdmitted={() => setAdmitting(undefined)} />
      <DrainDialog ask={draining} onClose={() => setDraining(undefined)} onDone={() => setDraining(undefined)} />
      <ForgetDialog node={forgetting} rotateOnForget={rotateOnForget} onClose={() => setForgetting(undefined)} onForgotten={() => undefined} />
    </div>
  )
}
