// The filter of the Events page in the address, so that a view of the events
// can be shared and survives a reload, and the query it makes of the daemon.

import type { EventFilter } from '../../app/EventsTable'

// The lists a filter has, in the order they are written in the address.
const lists = ['level', 'kind', 'route', 'guest', 'account'] as const

// The most events the daemon answers, and the page holds.
export const maxEvents = 5000

// filterOf reads the filter from the query of the address; a parameter that
// is empty is not set.
export function filterOf(search: string): EventFilter {
  const q = new URLSearchParams(search)
  const list = (key: string) => {
    const values = q.getAll(key).filter(Boolean)
    return values.length > 0 ? values : undefined
  }
  const one = (key: string) => q.get(key) || undefined
  return {
    level: list('level'),
    kind: list('kind'),
    route: list('route'),
    guest: list('guest'),
    account: list('account'),
    text: one('text'),
    since: one('since'),
    until: one('until'),
  }
}

// searchOf is the query of the address for a filter, without the question
// mark; empty for no filter.
export function searchOf(f: EventFilter): string {
  const q = new URLSearchParams()
  for (const key of lists) for (const value of f[key] ?? []) q.append(key, value)
  if (f.text) q.set('text', f.text)
  if (f.since) q.set('since', f.since)
  if (f.until) q.set('until', f.until)
  return q.toString()
}

export function isActive(f: EventFilter): boolean {
  return searchOf(f) !== ''
}

// rfc3339 is a time the way the daemon reads it, or nothing for what is not a
// time.
function rfc3339(at: string | undefined): string | undefined {
  const ms = Date.parse(at ?? '')
  return Number.isNaN(ms) ? undefined : new Date(ms).toISOString().replace('.000Z', 'Z')
}

// eventsQuery is the query of GET /v1/events for the newest limit events of
// the log that pass what the daemon can filter by. The text and the end of
// the range are the page's own to apply.
export function eventsQuery(f: EventFilter, limit: number): string {
  const q = new URLSearchParams({ history: '1', limit: String(limit) })
  for (const key of lists) for (const value of f[key] ?? []) q.append(key, value)
  const since = rfc3339(f.since)
  if (since) q.set('since', since)
  return q.toString()
}

// The events the daemon would answer the same for, whatever the limit: what
// the events already read are good for.
export function logKey(f: EventFilter): string {
  const q = new URLSearchParams(eventsQuery(f, 0))
  q.delete('limit')
  return q.toString()
}

const pad = (n: number) => String(n).padStart(2, '0')

// localInput is a time as the value of an input of type datetime-local, in
// the browser's zone; empty for what is not a time.
export function localInput(at: string | undefined): string {
  const ms = Date.parse(at ?? '')
  if (Number.isNaN(ms)) return ''
  const d = new Date(ms)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// fromLocalInput is the time of the value of such an input, as RFC 3339; the
// end of a range is the end of its minute, as the input has no seconds.
export function fromLocalInput(value: string, end = false): string | undefined {
  const ms = new Date(value).getTime()
  if (value === '' || Number.isNaN(ms)) return undefined
  return rfc3339(new Date(end ? ms + 59_999 : ms).toISOString())
}

// splitList is the values of a field that takes several, set apart by commas
// or spaces.
export function splitList(text: string): string[] {
  return text.split(/[\s,]+/).filter(Boolean)
}
