import './events.css'

import { useMemo, useState } from 'react'

import { api, ApiError } from '../../api/client'
import { explain } from '../../api/errors'
import { useApp } from '../../api/store'
import type { Event } from '../../api/types.gen'
import { type EventFilter, EventsTable, matches } from '../../app/EventsTable'
import { Head } from '../../app/Head'
import { navigate, useLocation } from '../../app/router'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { useToast } from '../../components/Toast'
import { Untrusted } from '../../components/Untrusted'
import { download, exportName } from './download'
import { EventFilters } from './EventFilters'
import { eventsQuery, filterOf, isActive, logKey, maxEvents, searchOf } from './filter'

// Each read of the log asks for this many more of the newest events.
const step = 1000

// What a read of the event log brought, for the filter it was made with.
interface Read {
  key: string
  limit: number
  events: Event[]
}

const failureOf = (e: unknown) => (e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: String(e) }))

function Failure({ error }: { error: ApiError }) {
  return (
    <p role="alert">
      <Untrusted text={explain(error).text} />
    </p>
  )
}

// EventsPage is the events of the daemon: the ones the page follows, and with
// Load older those of the event log on the node. The filters are in the
// address.
export function EventsPage() {
  const search = new URL(useLocation(), 'https://page.invalid').search
  const filter = filterOf(search)
  const key = logKey(filter)
  const held = useApp((s) => s.events)
  const node = useApp((s) => s.session?.node)
  const toast = useToast()
  const [live, setLive] = useState(true)
  const [read, setRead] = useState<Read>()
  const [reading, setReading] = useState(false)
  const [failure, setFailure] = useState<ApiError>()
  const [exporting, setExporting] = useState(false)

  // What was read for another filter is not what this one asks for.
  const older = read?.key === key ? read : undefined
  const kinds = useMemo(() => [...new Set([...held, ...(older?.events ?? [])].map((e) => e.kind))], [held, older])
  const everything = older !== undefined && older.events.length < older.limit
  const atMost = older !== undefined && older.limit >= maxEvents

  const change = (to: EventFilter) => {
    const query = searchOf(to)
    navigate(query ? `/events?${query}` : '/events', true)
  }

  const loadOlder = async () => {
    const limit = Math.min(maxEvents, (older?.limit ?? 0) + step)
    setReading(true)
    setFailure(undefined)
    try {
      const got = await api<Event[]>('GET', `/api/v1/events?${eventsQuery(filter, limit)}`)
      setRead({ key, limit, events: got ?? [] })
    } catch (e) {
      setFailure(failureOf(e))
    } finally {
      setReading(false)
    }
  }

  // The list as the filters make it, from the log: the table holds only what
  // the page has read.
  const exportList = async () => {
    setExporting(true)
    try {
      const got = await api<Event[]>('GET', `/api/v1/events?${eventsQuery(filter, maxEvents)}`)
      const list = (got ?? []).filter((e) => matches(e, filter))
      const name = exportName(node, new Date())
      download(name, `${JSON.stringify(list, null, 2)}\n`)
      toast(`Exported ${list.length === 1 ? '1 event' : `${list.length} events`} to ${name}.`, 'ok')
    } catch (e) {
      toast(<Untrusted text={`The events were not exported: ${explain(failureOf(e)).text}`} />, 'fail')
    } finally {
      setExporting(false)
    }
  }

  return (
    <>
      <Head
        title="Events"
        description="The daemon keeps the last thousand events in memory; the journal on the node has them all."
        actions={
          <>
            <label className="switch">
              <input type="checkbox" role="switch" checked={live} onChange={(e) => setLive(e.target.checked)} />
              Live
            </label>
            {exporting ? <Busy label="Reading the event log" /> : <Button onClick={() => void exportList()}>Export the list</Button>}
          </>
        }
      />
      <EventFilters filter={filter} kinds={kinds} onChange={change} />
      {!live && <p className="muted">Paused: the events that come now are not added to the list until Live is on.</p>}
      <EventsTable
        filter={filter}
        live={live}
        older={older?.events}
        empty={
          isActive(filter)
            ? 'No event among those the page has read passes these filters. Load older events reads the event log on the node.'
            : undefined
        }
      />
      <div className="events-more">
        {reading ? (
          <Busy label="Reading the event log" />
        ) : (
          <Button
            onClick={() => void loadOlder()}
            disabledReason={
              everything
                ? 'That is every event the log has for these filters.'
                : atMost
                  ? `The page reads ${maxEvents} events at most. Narrow the filters, or read the journal on the node.`
                  : undefined
            }
          >
            Load older events
          </Button>
        )}
        {failure && <Failure error={failure} />}
      </div>
      <p className="muted">
        Times are in your browser&apos;s time zone. Load older events reads the event log on the node, {step} events more each time, up to {maxEvents}. Export reads it with
        these filters, {maxEvents} events at most.
      </p>
    </>
  )
}
