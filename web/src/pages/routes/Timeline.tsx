import { useEffect, useState } from 'react'

import { api, ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { Event } from '../../api/types.gen'
import { Button } from '../../components/Button'
import { RefreshIcon } from '../../components/icons'
import { Skeleton } from '../../components/Skeleton'
import { LevelBadge } from '../../components/StateBadge'
import { Time } from '../../components/Time'
import { Untrusted } from '../../components/Untrusted'
import { ErrorText } from './parts'

// How many events of the history are read; the journal has them all.
export const timelineLimit = 500

const eventKey = (e: Event) => `${e.boot ?? ''}:${e.seq}`

// timelineOf is the history read for a route and the events the stream has
// brought since, each once, the newest first. Events of other boots have
// other numbers: they are ordered by time.
export function timelineOf(read: readonly Event[], live: readonly Event[], hostname: string): Event[] {
  const byKey = new Map<string, Event>()
  for (const e of [...read, ...live.filter((e) => e.route === hostname)]) byKey.set(eventKey(e), e)
  const at = (e: Event) => Date.parse(e.at ?? '') || 0
  return [...byKey.values()].sort((a, b) => at(b) - at(a) || b.seq - a.seq)
}

// Timeline is the history of a route: its states, the claims on its
// hostname and the actions made for it, with the admin who made them. The
// events log is read once; what the stream brings after is added.
export function Timeline({ hostname }: { hostname: string }) {
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const live = useApp((s) => s.events)
  const [read, setRead] = useState<Event[] | ApiError | undefined>(undefined)
  const [asked, setAsked] = useState(0)

  useEffect(() => {
    let on = true
    const path = `/api/v1/events?route=${encodeURIComponent(hostname)}&history=1&limit=${timelineLimit}`
    api<Event[]>('GET', path, undefined, { background: true }).then(
      (events) => on && setRead(events ?? []),
      (e: unknown) => on && setRead(e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: String(e) })),
    )
    return () => {
      on = false
    }
  }, [hostname, asked])

  if (read === undefined) return <Skeleton lines={4} label="Loading the history of the route" />
  if (read instanceof ApiError) {
    return (
      <>
        <ErrorText error={read} />
        <Button small icon={<RefreshIcon />} onClick={() => setAsked(asked + 1)}>
          Try again
        </Button>
      </>
    )
  }
  const events = timelineOf(read, live, hostname)
  if (events.length === 0) return <p className="muted">No events of this route, in memory or in events.log.</p>
  // A list rather than a table: the drawer is narrow, and a message is read
  // in full.
  return (
    <ol className="timeline" aria-label="History of the route">
      {events.map((e) => (
        <li key={eventKey(e)} className={e.level === 'error' ? 'timeline-entry timeline-error' : 'timeline-entry'}>
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
