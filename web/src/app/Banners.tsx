import type { ReactNode } from 'react'

import { type AppState, dataAge, lastCycle, monoNow, useApp } from '../api/store'
import { tooManyText } from '../api/stream'
import { Banner } from '../components/Banner'
import { CopyCommand } from '../components/CopyCommand'
import { Time } from '../components/Time'
import { Untrusted } from '../components/Untrusted'
import { durationText } from '../text/duration'
import { egressLoadCommand, egressOnCommand, writerText } from '../text/words'
import { useNow } from './clock'
import { Link } from './Link'

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
// monotonic clock, mono.
export function bannersOf(s: AppState, mono: number = monoNow()): BannerSpec[] {
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

  if (ran && st.mode === 'observe') {
    out.push({
      key: 'observe',
      tone: 'info',
      text: 'pco observes only: it plans, and changes nothing at Cloudflare until publishing is started.',
      action: review('/routes/plan', 'Review plan'),
    })
  }
  if (st && st.credentials.length === 0) {
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

export function Banners() {
  const app = useApp((s) => s)
  // again every second, for the age of the data
  useNow()
  const banners = bannersOf(app)
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
