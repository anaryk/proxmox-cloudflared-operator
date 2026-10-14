import type { JSX } from 'react'

import { useApp } from '../api/store'
import type { AgentView } from '../api/types.gen'
import { Head } from '../app/Head'
import { Link } from '../app/Link'
import { Skeleton } from '../components/Skeleton'
import { StatusBadge } from '../components/StateBadge'
import { Time } from '../components/Time'
import { Untrusted } from '../components/Untrusted'
import { durationText, nanosToMs } from '../text/duration'
import { Card, Failure, useResource } from './kit'
import { notController } from './nodes/nodes'

// How long without a heartbeat the agent counts the controller silent.
const silentAfterMs = 60_000

const age = (ns: number) => `${durationText(nanosToMs(ns))} ago`

// Agent is what the agent of this node says of itself: whom it follows,
// at which epoch, what it applied last and what it runs.
export function Agent({ agent }: { agent: AgentView }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const silent = nanosToMs(agent.silentFor) >= silentAfterMs
  const replicas = Object.entries(agent.readyReplicas ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
  return (
    <dl className="details">
      <dt>Node</dt>
      <dd>
        <Untrusted className="mono" text={agent.node} /> · <Untrusted text={agent.kind} />
      </dd>
      <dt>Controller</dt>
      <dd>
        {agent.controller ? <Untrusted className="mono" text={agent.controller} /> : <span className="muted">none followed</span>}
        {agent.stoodDown ? (
          <p>
            <StatusBadge tone="fail">stood down</StatusBadge> <Untrusted text={agent.stoodDown} />
          </p>
        ) : (
          <p>
            <StatusBadge tone={silent || !agent.heard ? 'warn' : 'ok'}>{!agent.heard ? 'not heard' : silent ? 'silent' : 'heard'}</StatusBadge>{' '}
            <span className="muted">
              {agent.heard ? `last heartbeat ${age(agent.heartbeatAge)}` : 'no heartbeat came yet'}
              {silent && `; the controller has been silent for ${durationText(nanosToMs(agent.silentFor))}`}
            </span>
          </p>
        )}
      </dd>
      <dt>Epoch</dt>
      <dd className="mono">
        {agent.epoch.generation}.<Untrusted text={agent.epoch.nonce} /> <span className="muted">of install</span> <Untrusted text={agent.epoch.installId} />
      </dd>
      <dt>Desired state</dt>
      <dd>
        sequence <span className="num">{agent.seq}</span>, applied {age(agent.seqAge)}
      </dd>
      <dt>Runs</dt>
      <dd>
        {agent.connectors === 1 ? '1 connector' : `${agent.connectors} connectors`}; {agent.egressTargets === 1 ? '1 target' : `${agent.egressTargets} targets`} in its egress set
      </dd>
      {replicas.length > 0 && (
        <>
          <dt>Ready replicas</dt>
          <dd>
            <ul className="plain-list">
              {replicas.map(([tunnel, n]) => (
                <li key={tunnel}>
                  <Untrusted className="mono" text={tunnel.slice(0, 8)} />: {n} as the last heartbeat counted them
                </li>
              ))}
            </ul>
          </dd>
        </>
      )}
      {agent.certFingerprint && (
        <>
          <dt>Certificate</dt>
          <dd>
            <span className="mono">
              <Untrusted text={agent.certFingerprint} />
            </span>
            {agent.certExpires && (
              <>
                , expires <Time at={agent.certExpires} nodeZone={nodeZone} />
              </>
            )}
            <p className="field-hint">The fingerprint an admin types to admit an appliance agent.</p>
          </dd>
        </>
      )}
      {agent.problems.length > 0 && (
        <>
          <dt>Problems</dt>
          <dd>
            <ul className="plain-list">
              {agent.problems.map((p) => (
                <li key={p}>
                  <Untrusted text={p} />
                </li>
              ))}
            </ul>
          </dd>
        </>
      )}
    </dl>
  )
}

// LocalAgent is /agent: the agent of this node as it sees itself, which
// every node answers for itself, whatever its role. On a node that does
// not hold the lease it is what the page can show: the controller's pages
// are served on the controller. The agent's view is for admins only, as it
// names the certificate and the controller it follows; a reader gets the
// lease from the state and is not sent for a refusal.
export function LocalAgent(): JSX.Element {
  const st = useApp((s) => s.state)
  const digest = useApp((s) => s.state?.digest)
  const admin = useApp((s) => s.session?.role === 'admin')
  const got = useResource<AgentView>(admin ? '/api/v1/local/agent' : undefined, digest)
  return (
    <>
      <Head title="This node" description="The agent of this node as it sees itself: whom it follows, what it applied last and what it runs." />
      {st && notController(st) && (
        <p className="muted">
          This node does not hold the lease
          {st.lease?.holder ? (
            <>
              : <Untrusted className="mono" text={st.lease.holder} /> is the controller
            </>
          ) : (
            ', and no controller is known'
          )}
          . Every action is refused here; the <Link to="/nodes">nodes</Link> and the routes are served as this node last saw them.
        </p>
      )}
      <Card id="agent-self" title="Agent">
        {!admin ? (
          <p>This page is for admins: the agent of this node names its certificate and the controller it follows.</p>
        ) : got.error ? (
          <Failure error={got.error} onTryAgain={got.reload} />
        ) : got.data ? (
          <Agent agent={got.data} />
        ) : (
          <Skeleton lines={6} label="Loading the agent" />
        )}
      </Card>
    </>
  )
}
