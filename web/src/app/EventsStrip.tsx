import { useLayoutEffect, useState } from 'react'

import { useApp } from '../api/store'
import { EventsIcon } from '../components/icons'
import { Time } from '../components/Time'
import { Untrusted } from '../components/Untrusted'
import { EventsTable, hasError, rowsOf } from './EventsTable'
import { Link } from './Link'

// How many rows the strip shows.
const stripRows = 5

const noneLoaded = new Map()

// The strip's height, which the page keeps what takes the focus clear of
// (scroll-padding in shell.css): it grows when the strip is opened.
function useHeightOnPage(el: HTMLElement | null): void {
  useLayoutEffect(() => {
    if (!el) return
    const root = document.documentElement
    const measure = () => root.style.setProperty('--strip-height', `${el.offsetHeight}px`)
    measure()
    if (typeof ResizeObserver === 'undefined') return () => root.style.removeProperty('--strip-height')
    const watch = new ResizeObserver(measure)
    watch.observe(el)
    return () => {
      watch.disconnect()
      root.style.removeProperty('--strip-height')
    }
  }, [el])
}

// EventsStrip is the panel at the foot of every page, after the task log of
// Proxmox VE: the latest events, live, the newest in its line while it is
// closed. An error among them, a gap's included, colours it.
export function EventsStrip() {
  const events = useApp((s) => s.events)
  const gaps = useApp((s) => s.gaps)
  const nodeZone = useApp((s) => s.session?.nodeZone)
  const [open, setOpen] = useState(false)
  const [strip, setStrip] = useState<HTMLDetailsElement | null>(null)
  useHeightOnPage(strip)
  const latest = rowsOf(events, gaps, noneLoaded, {}).slice(0, stripRows)
  const error = hasError(latest)
  const last = events.at(-1)
  return (
    <details ref={setStrip} className={error ? 'strip strip-error' : 'strip'} open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary>
        <EventsIcon />
        <b>Events</b>
        {error && <span className="strip-flag">errors</span>}
        <span className="strip-last">
          {last ? (
            <>
              <Time at={last.at ?? ''} nodeZone={nodeZone} /> <Untrusted text={last.subject} max={60} />: <Untrusted text={last.message} max={120} />
            </>
          ) : (
            <span className="muted">No events yet</span>
          )}
        </span>
        <span className="strip-open muted">{open ? 'close' : 'open'}</span>
      </summary>
      {open && (
        <div className="strip-body">
          <EventsTable filter={{}} live rows={stripRows} />
          <Link to="/events" className="strip-more">
            All events
          </Link>
        </div>
      )}
    </details>
  )
}
