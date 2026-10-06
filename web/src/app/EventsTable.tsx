import { Fragment, type JSX, useState } from 'react'

import { api, ApiError } from '../api/client'
import { explain } from '../api/errors'
import { useApp } from '../api/store'
import type { Event, GapNotice } from '../api/types.gen'
import { Drawer } from '../components/Drawer'
import { LevelBadge } from '../components/StateBadge'
import { type Column, headHeight, Table } from '../components/Table'
import { Time, TimeLines } from '../components/Time'
import { Untrusted } from '../components/Untrusted'
import { rowHeights, usePreferences } from '../theme/theme'

export interface EventFilter {
  route?: string[]
  guest?: string[]
  account?: string[]
  kind?: string[]
  level?: string[]
  text?: string
}

// The kinds the stream coalesces into a gap.
const gapKinds = ['route', 'action', 'claim']

const has = (want: string[] | undefined, value: string | undefined) => !want || want.length === 0 || (value !== undefined && want.includes(value))

// matches is the filter of the Events page, as the daemon's query has it:
// within a list one value, across them every list; text in any field.
export function matches(e: Event, f: EventFilter): boolean {
  if (!has(f.route, e.route) || !has(f.guest, e.guest) || !has(f.account, e.account) || !has(f.kind, e.kind) || !has(f.level, e.level)) {
    return false
  }
  const text = f.text?.trim().toLowerCase()
  if (!text) return true
  return [e.subject, e.message, e.route, e.guest, e.tunnel, e.account, e.actor, e.kind].some((v) => v?.toLowerCase().includes(text))
}

// A gap can hold any route, guest or account; it shows while the filter
// names none, nor text, and could hold the kind and level asked for.
function gapMatches(f: EventFilter): boolean {
  if (f.route?.length || f.guest?.length || f.account?.length || f.text?.trim()) return false
  if (f.kind?.length && !f.kind.some((k) => gapKinds.includes(k))) return false
  return true
}

// What was loaded of a gap: the events it stood for, and whether they are
// all of them. The daemon answers the newest events after a seq, so what it
// sends may begin after the gap's first, when more came since than it gives
// at once or the gap is older than what it keeps in memory.
export interface Part {
  events: Event[]
  complete: boolean
}

type Loaded = Part | 'loading' | ApiError

const isPart = (l: Loaded | undefined): l is Part => typeof l === 'object' && !(l instanceof ApiError)

type Row = { type: 'event'; e: Event } | { type: 'gap'; g: GapNotice; loaded?: Loaded }

const gapKey = (g: GapNotice) => `gap:${g.boot}:${g.from}:${g.to}`
const rowKey = (r: Row) => (r.type === 'gap' ? gapKey(r.g) : `event:${r.e.boot ?? ''}:${r.e.seq}`)
const seqOf = (r: Row) => (r.type === 'gap' ? r.g.to : r.e.seq)

// rowsOf are the events and the gaps, newest first; a gap that was opened
// is the events it stood for.
export function rowsOf(events: readonly Event[], gaps: readonly GapNotice[], loaded: ReadonlyMap<string, Loaded>, f: EventFilter, upTo?: number): Row[] {
  const rows: Row[] = []
  const seen = new Set<number>()
  for (const e of events) {
    seen.add(e.seq)
    if (matches(e, f)) rows.push({ type: 'event', e })
  }
  for (const g of gaps) {
    const l = loaded.get(gapKey(g))
    if (isPart(l)) {
      for (const e of l.events) if (!seen.has(e.seq) && matches(e, f)) rows.push({ type: 'event', e })
      if (l.complete) continue
    }
    if (gapMatches(f) && has(f.level, g.level)) rows.push({ type: 'gap', g, loaded: l })
  }
  return rows.filter((r) => upTo === undefined || seqOf(r) <= upTo).sort((a, b) => seqOf(b) - seqOf(a))
}

// hasError says whether a row is of level error, which colours the strip.
export function hasError(rows: readonly Row[]): boolean {
  return rows.some((r) => (r.type === 'gap' ? r.g.level : r.e.level) === 'error')
}

function GapCell({ row }: { row: Extract<Row, { type: 'gap' }> }) {
  const n = row.g.count
  if (row.loaded === 'loading') return <span role="status">Loading {n} events…</span>
  if (row.loaded instanceof ApiError) {
    return (
      <span>
        Could not load them: <Untrusted text={explain(row.loaded).text} />
      </span>
    )
  }
  if (isPart(row.loaded)) {
    return (
      <span>
        {row.loaded.events.length} of {n} events of one cycle loaded: the daemon no longer holds the rest, the journal on the node has them.{' '}
        <span className="muted">(open to try again)</span>
      </span>
    )
  }
  return (
    <span>
      {n === 1 ? '1 event' : `${n} events`} of one cycle <span className="muted">(open to load them)</span>
    </span>
  )
}

// fetchGap asks the daemon for the events of a gap: the newest after the one
// before it, as many as there have been since and a margin for those the
// stream has not brought yet; with history, from its log on disk too.
async function fetchGap(g: GapNotice, lastSeq: number, history: boolean): Promise<{ inGap: Event[]; first?: number }> {
  const limit = history ? 5000 : Math.min(5000, Math.max(lastSeq, g.to) - g.from + 1 + 500)
  const got = await api<Event[]>('GET', `/api/v1/events?after=${g.from - 1}&boot=${encodeURIComponent(g.boot)}&limit=${limit}${history ? '&history=1' : ''}`)
  const mine = (got ?? []).filter((e) => e.boot === g.boot)
  return {
    inGap: mine.filter((e) => e.seq >= g.from && e.seq <= g.to),
    first: mine.length > 0 ? Math.min(...mine.map((e) => e.seq)) : undefined,
  }
}

function EventDetail({ e, nodeZone, onClose }: { e: Event; nodeZone?: string; onClose: () => void }) {
  const fields: [string, string | undefined][] = [
    ['Kind', e.kind],
    ['Subject', e.subject],
    ['Route', e.route],
    ['Guest', e.guest],
    ['Tunnel', e.tunnel],
    ['Account', e.account],
    ['Actor', e.actor],
  ]
  return (
    <Drawer open onClose={onClose} title={`Event ${e.seq}`}>
      <dl className="details">
        <TimeLines at={e.at ?? ''} nodeZone={nodeZone} />
        <dt>Level</dt>
        <dd>
          <LevelBadge level={e.level} />
        </dd>
        {fields.map(([name, value]) =>
          value ? (
            <Fragment key={name}>
              <dt>{name}</dt>
              <dd>
                <Untrusted text={value} />
              </dd>
            </Fragment>
          ) : null,
        )}
        <dt>Message</dt>
        <dd>
          <Untrusted text={e.message} />
        </dd>
      </dl>
    </Drawer>
  )
}

// EventsTable is the one table of events: the strip, the Overview, the
// Events page, a route's timeline and a guest's detail. It
// has the events the page holds, the newest first; live off holds the rows
// as they were. A row opens its detail, a gap loads the events it stands for.
export function EventsTable({ filter, live, rows }: { filter: EventFilter; live: boolean; rows?: number }): JSX.Element {
  const events = useApp((s) => s.events)
  const gaps = useApp((s) => s.gaps)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const lastSeq = useApp((s) => Math.max(s.hello?.seq ?? 0, s.events.at(-1)?.seq ?? 0, s.gaps.at(-1)?.to ?? 0))
  const { density } = usePreferences()
  const [loaded, setLoaded] = useState<ReadonlyMap<string, Loaded>>(new Map())
  const [open, setOpen] = useState<Event>()
  // Paused: the rows up to the last event there was when it paused.
  const [pausedAt, setPausedAt] = useState<number | undefined>(live ? undefined : lastSeq)
  if (!live && pausedAt === undefined) setPausedAt(lastSeq)
  if (live && pausedAt !== undefined) setPausedAt(undefined)

  const shown = rowsOf(events, gaps, loaded, filter, live ? undefined : pausedAt)

  // load loads a gap: from what the daemon holds in memory, then, when that
  // does not reach back to the gap's first event, from its log as well. A
  // gap that is still not whole says how much of it there is, and loads
  // again when opened again.
  const load = async (g: GapNotice) => {
    const key = gapKey(g)
    const now = loaded.get(key)
    if (now === 'loading' || (isPart(now) && now.complete)) return
    setLoaded((m) => new Map(m).set(key, 'loading'))
    const known = new Set(events.map((e) => e.seq))
    try {
      let got = isPart(now) ? undefined : await fetchGap(g, lastSeq, false)
      if (got?.first === undefined || got.first > g.from) got = await fetchGap(g, lastSeq, true)
      const complete = got.first !== undefined && got.first <= g.from
      setLoaded((m) => new Map(m).set(key, { events: got.inGap.filter((e) => !known.has(e.seq)), complete }))
    } catch (e) {
      setLoaded((m) => new Map(m).set(key, e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: String(e) })))
    }
  }

  const columns: Column<Row>[] = [
    {
      key: 'time',
      header: 'Time',
      className: 'col-time',
      cell: (r) => (r.type === 'event' ? <Time at={r.e.at ?? ''} nodeZone={nodeZone} /> : null),
    },
    { key: 'level', header: 'Level', className: 'col-level', cell: (r) => <LevelBadge level={r.type === 'gap' ? r.g.level : r.e.level} /> },
    { key: 'kind', header: 'Kind', className: 'col-kind', cell: (r) => (r.type === 'gap' ? <span className="muted">route, action, claim</span> : <Untrusted text={r.e.kind} />) },
    { key: 'subject', header: 'Subject', lead: true, cell: (r) => (r.type === 'gap' ? <GapCell row={r} /> : <Untrusted text={r.e.subject} max={80} />) },
    { key: 'message', header: 'Message', cell: (r) => (r.type === 'gap' ? null : <Untrusted text={r.e.message} max={200} />) },
    { key: 'actor', header: 'Actor', className: 'col-actor', cell: (r) => (r.type === 'gap' || !r.e.actor ? null : <Untrusted text={r.e.actor} />) },
  ]

  const height = rows === undefined ? undefined : headHeight + rows * rowHeights[density] + 2
  return (
    <div className="events">
      <Table
        label="Events"
        columns={columns}
        rows={shown}
        rowKey={rowKey}
        height={height}
        current={open ? `event:${open.boot ?? ''}:${open.seq}` : undefined}
        onActivate={(r) => (r.type === 'gap' ? void load(r.g) : setOpen(r.e))}
        rowClass={(r) => ((r.type === 'gap' ? r.g.level : r.e.level) === 'error' ? 'row-error' : undefined)}
        empty="No events yet. The daemon keeps the last thousand in memory; the journal has them all."
      />
      {open && <EventDetail e={open} nodeZone={nodeZone} onClose={() => setOpen(undefined)} />}
    </div>
  )
}
