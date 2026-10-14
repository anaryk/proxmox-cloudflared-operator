import type { ReactNode } from 'react'

import { type AppState, dataAge, lastCycle, monoNow, useApp } from '../api/store'
import { tooManyText } from '../api/stream'
import type { State } from '../api/types.gen'
import { Banner } from '../components/Banner'
import { CopyCommand } from '../components/CopyCommand'
import { Time } from '../components/Time'
import { Untrusted } from '../components/Untrusted'
import { takeoverHold } from '../pages/nodes/nodes'
import { durationText } from '../text/duration'
import { egressLoadCommand, egressOnCommand, promoteCommand, rotationText, writerText } from '../text/words'
import { useNow } from './clock'
import { Link } from './Link'
import { useView } from './router'

export interface BannerSpec {
  key: string
  tone: 'fail' | 'warn' | 'info'
  text: ReactNode
  action?: ReactNode
}

const zeroTime = (at?: string) => !at || at.startsWith('0001-01-01T00:00:00')

const review = (to: string, label = 'Review') => (
  <Link to={to} className="btn btn-small">
    {label}
  </Link>
)

// bannersOf are the banners under the top bar, most severe first, each with
// one action at most. The age of the data is measured on the browser's
// monotonic clock, mono. page is the name of the view shown: the banner that
// points at the setup is not shown on the setup itself.
export function bannersOf(s: AppState, mono: number = monoNow(), page?: string): BannerSpec[] {
  const zone = s.session?.nodeZone
  const at = (t: string) => <Time at={t} nodeZone={zone} />
  const st = s.state
  const ran = st !== undefined && !zeroTime(st.at)
  const out: BannerSpec[] = []

  if (s.conn === 'web-down') {
    out.push({
      key: 'web',
      tone: 'fail',
      text: <>This page cannot reach pco&apos;s web process (since {at(s.connSince ?? '')}). Check that pco-web.service runs on the node.</>,
    })
  }
  if (s.conn === 'daemon-down') {
    out.push({
      key: 'daemon',
      tone: 'fail',
      text: <>The pco daemon does not answer (since {at(s.upstream.since)}). Published routes keep working; nothing changes until it is back.</>,
    })
  }

  const egress = st?.egress
  if (egress?.state === 'off') {
    out.push({
      key: 'egress',
      tone: 'fail',
      text: (
        <>
          The egress filter is switched off{egress.since && <> since {at(egress.since)}</>}: the connectors are not confined.
          <CopyCommand cmd={egressOnCommand} root />
        </>
      ),
    })
  } else if (egress?.state === 'not loaded' || egress?.state === 'changed') {
    out.push({
      key: 'egress',
      tone: 'fail',
      text: (
        <>
          {egress.state === 'not loaded'
            ? 'The egress filter is on, but its table is not loaded: the connectors are not confined.'
            : 'The egress filter is on, but its table is not as pco loads it, and the connectors may not be confined.'}
          <CopyCommand cmd={egressLoadCommand} root />
        </>
      ),
    })
  }

  if (ran && st.writerVerdict !== 'ok' && st.writerVerdict !== 'unknown' && st.writerVerdict !== '') {
    out.push({ key: 'writer', tone: 'fail', text: <>Writer: {writerText(st.writerVerdict)}. pco doctor on the node says more.</> })
  }
  out.push(...clusterFailures(st, at))

  if (s.conn === 'stale') {
    const from = lastCycle(s)
    const age = dataAge(s, mono)
    const reconnecting = s.link.state === 'reconnecting' || s.link.state === 'too-many'
    out.push({
      key: 'stale',
      tone: 'warn',
      text: (
        <>
          {from ? (
            <>
              The data is from {at(from)}
              {age !== undefined && `, ${durationText(age)} ago`}:{' '}
            </>
          ) : (
            'The page has no data yet: '
          )}
          {reconnecting ? 'the stream of pco web is reconnecting.' : 'no cycle of the daemon has finished since.'} Live figures are greyed out until fresh data
          arrives.
        </>
      ),
    })
  }
  if (s.link.state === 'too-many') {
    out.push({ key: 'tabs', tone: 'warn', text: `${tooManyText}; this page tries again in ${s.link.retryAfter} s.` })
  }

  if (st?.hold) {
    out.push({
      key: 'hold',
      tone: 'warn',
      text: (
        <>
          The last cycle held (<Untrusted text={st.hold} />
          ): these are the routes of an earlier cycle.
        </>
      ),
    })
  }
  if (ran && !st.complete) {
    out.push({ key: 'inventory', tone: 'warn', text: 'The inventory is incomplete: these are the routes of an earlier cycle.' })
  }
  out.push(...clusterWarnings(st, at))

  const waiting = st?.waiting ?? []
  const [first] = waiting
  if (first) {
    out.push({
      key: 'waiting',
      tone: 'warn',
      text: (
        <>
          <b>{waiting.length === 1 ? '1 change waits' : `${waiting.length} changes wait`} for a confirmation.</b> <Untrusted text={first.detail} />
          {waiting.length > 1 && ` (and ${waiting.length - 1} more)`}
        </>
      ),
      action: review('/routes/plan'),
    })
  }

  const unapproved = st?.unapproved ?? []
  const [guest] = unapproved
  if (guest) {
    out.push({
      key: 'approval',
      tone: 'info',
      text:
        unapproved.length === 1 ? (
          <>
            <b>1 guest waits for approval:</b>{' '}
            <span className="mono">
              {guest.kind}/{guest.vmid}
            </span>
            {guest.name && (
              <>
                {' '}
                <Untrusted text={guest.name} />
              </>
            )}{' '}
            would publish{' '}
            {guest.hostnames.map((h, i) => (
              <span key={h}>
                {i > 0 && ', '}
                <Untrusted text={h} hostname />
              </span>
            ))}
            .
          </>
        ) : (
          <b>{unapproved.length} guests wait for approval.</b>
        ),
      action: review('/guests'),
    })
  }

  out.push(...clusterNotices(st, at))
  if (ran && st.mode === 'observe') {
    out.push({
      key: 'observe',
      tone: 'info',
      text: 'pco observes only: it plans, and changes nothing at Cloudflare until publishing is started.',
      action: review('/routes/plan', 'Review plan'),
    })
  }
  if (st && st.credentials.length === 0 && page !== 'setup') {
    out.push({ key: 'credential', tone: 'info', text: 'pco has no Cloudflare API token yet.', action: review('/setup', 'Open the setup') })
  }
  if (s.skew) {
    out.push({
      key: 'version',
      tone: 'info',
      text: 'pco was updated; reload to use it.',
      action: (
        <button type="button" className="btn btn-small" onClick={() => window.location.reload()}>
          Reload
        </button>
      ),
    })
  }
  return out
}

const names = (list: readonly string[] | undefined) => (list ?? []).join(', ')
const nodeWord = (list: readonly string[] | undefined) => ((list ?? []).length === 1 ? 'Node' : 'Nodes')
const nodes = (to: string) => review(to, 'Nodes')

// The banners of a cluster that stop every write: no quorum, a lease that
// is fenced, a controller that is behind the agents, and this node not
// holding the lease, which makes every action of the page fail here.
function clusterFailures(st: State | undefined, at: (t: string) => ReactNode): BannerSpec[] {
  const out: BannerSpec[] = []
  const lease = st?.lease
  const cluster = st?.cluster
  if (cluster && !cluster.quorate) {
    out.push({ key: 'quorum', tone: 'fail', text: 'The cluster is not quorate: nothing is written until it is.', action: nodes('/nodes') })
  }
  if (lease && lease.holder === st?.node && lease.fenced) {
    out.push({
      key: 'fenced',
      tone: 'fail',
      text: (
        <>
          The lease is fenced: <Untrusted text={lease.fenced} />. Nothing is written.
        </>
      ),
      action: nodes('/nodes'),
    })
  }
  if (lease?.behind) {
    out.push({
      key: 'behind',
      tone: 'fail',
      text: (
        <>
          The controller is behind what the agents remember: <Untrusted text={lease.behind} />. Nothing is written until pco promote adopts the store, as root on the
          controller.
          <CopyCommand cmd={promoteCommand} root />
        </>
      ),
    })
  }
  if (lease && !lease.valid && lease.holder !== st?.node) {
    out.push({
      key: 'controller',
      tone: 'warn',
      text: lease.holder ? (
        <>
          This node does not hold the lease: node <Untrusted className="mono" text={lease.holder} />
          {lease.address && (
            <>
              {' '}
              (<Untrusted className="mono" text={lease.address} />)
            </>
          )}{' '}
          is the controller{lease.acquiredAt && <> since {at(lease.acquiredAt)}</>}. This page shows what this node&apos;s agent knows, and every action is refused here.
        </>
      ) : (
        <>No controller is known: no node holds the lease now. This page shows what this node&apos;s agent knows, and every action is refused here.</>
      ),
      action: review('/agent', 'This node'),
    })
  }
  return out
}

// The banners of a cluster that hold or demote something: a takeover that
// waits for the agents, coverage lost, a rotation under way or refusing
// nodes, and addresses blocked on every node.
function clusterWarnings(st: State | undefined, at: (t: string) => ReactNode): BannerSpec[] {
  const out: BannerSpec[] = []
  const hold = takeoverHold(st)
  if (hold) {
    out.push({
      key: 'takeover',
      tone: 'warn',
      text: (
        <>
          The controller took over: <Untrusted text={hold} />.
        </>
      ),
    })
  }
  const cluster = st?.cluster
  if (!cluster) return out
  const c = cluster.coverage
  if (!c.covered) {
    out.push({
      key: 'coverage',
      tone: 'warn',
      text: (
        <>
          {(c.uncovered ?? []).length > 0 ? (
            <>
              {nodeWord(c.uncovered)} <Untrusted className="mono" text={names(c.uncovered)} /> {(c.uncovered ?? []).length === 1 ? 'has' : 'have'} no pco agent with a running watch
            </>
          ) : (
            'Not every node watches its forwarding table'
          )}
          {c.since && <> since {at(c.since)}</>}: routes of other nodes&apos; guests are pinned to their nodes.
          {(c.flapping ?? []).length > 0 && (
            <>
              {' '}
              The watch of <Untrusted className="mono" text={names(c.flapping)} /> broke twice within ten minutes: those routes stay pinned until it runs for ten minutes.
            </>
          )}
        </>
      ),
      action: nodes('/nodes'),
    })
  }
  const rotation = cluster.caRotation
  if (rotation && (rotation.next || (rotation.refused ?? []).length > 0)) {
    out.push({
      key: 'rotation',
      tone: 'warn',
      text: (
        <>
          Every secret of the install is rotated after a node was forgotten as compromised: <Untrusted text={rotationText(rotation)} />.
          {(rotation.refused ?? []).length > 0 && (
            <>
              {' '}
              The new CA refuses <Untrusted className="mono" text={names(rotation.refused)} />: each issues its certificate again at its next start of pco.
            </>
          )}
        </>
      ),
      action: nodes('/nodes'),
    })
  }
  const blocks = cluster.blocks ?? []
  const [block] = blocks
  if (block) {
    out.push({
      key: 'blocks',
      tone: 'warn',
      text:
        blocks.length === 1 ? (
          <>
            <Untrusted className="mono" text={block.address} /> is blocked on every node: the MAC of guest <Untrusted className="mono" text={block.guest} /> was seen on port{' '}
            <Untrusted className="mono" text={block.port} /> of node <Untrusted className="mono" text={block.node} />.
          </>
        ) : (
          <>{blocks.length} addresses are blocked on every node: each guest&apos;s MAC was seen on the port of another guest.</>
        ),
      action: review('/nodes', 'Blocked addresses'),
    })
  }
  return out
}

// The banners of a cluster that say what happened: the controller moved, a
// hand-off is under way, an upgrade waits to be finished.
function clusterNotices(st: State | undefined, at: (t: string) => ReactNode): BannerSpec[] {
  const out: BannerSpec[] = []
  const lease = st?.lease
  if (lease?.previous && lease.holder) {
    out.push({
      key: 'moved',
      tone: 'info',
      text: (
        <>
          The controller moved from <Untrusted className="mono" text={lease.previous} /> to <Untrusted className="mono" text={lease.holder} />, generation {lease.generation}
          {lease.acquiredAt && <>, at {at(lease.acquiredAt)}</>}.
        </>
      ),
      action: nodes('/nodes'),
    })
  }
  if (lease?.handoff) {
    out.push({
      key: 'handoff',
      tone: 'info',
      text: (
        <>
          The lease is being handed off to node <Untrusted className="mono" text={lease.handoff} />: the next cycle runs there, and this page sends you to it.
        </>
      ),
    })
  }
  const upgrade = st?.cluster?.upgrade
  if (upgrade) {
    out.push({
      key: 'upgrade',
      tone: 'info',
      text: (
        <>
          An upgrade from release <Untrusted text={upgrade.from} /> is unfinished{upgrade.since && <> since {at(upgrade.since)}</>}; pco cluster finish-upgrade on the
          controller ends it once every node runs this release
          {(upgrade.older ?? []).length > 0 && (
            <>
              {' '}
              (still older: <Untrusted className="mono" text={names(upgrade.older)} />)
            </>
          )}
          . The settings of the cluster cannot be changed until then.
        </>
      ),
      action: nodes('/nodes'),
    })
  }
  return out
}

export function Banners() {
  const app = useApp((s) => s)
  // again every second, for the age of the data
  useNow()
  const view = useView()
  const banners = bannersOf(app, monoNow(), view.name)
  if (banners.length === 0) return null
  return (
    <div className="banners">
      {banners.map((b) => (
        <div key={b.key} data-banner={b.key} role={b.tone === 'fail' ? 'alert' : undefined}>
          <Banner tone={b.tone} action={b.action}>
            {b.text}
          </Banner>
        </div>
      ))}
    </div>
  )
}
