import { Tooltip } from './Tooltip'

interface Parts {
  date: string
  time: string
  offset: string
}

// The formats of named zones. The browser's own is looked up each time, as
// the system may change it while the page is open.
const formats = new Map<string, Intl.DateTimeFormat>()

function formatIn(timeZone: string | undefined): Intl.DateTimeFormat {
  const cached = timeZone && formats.get(timeZone)
  if (cached) return cached
  const format = new Intl.DateTimeFormat('en-US', {
    timeZone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hourCycle: 'h23',
    timeZoneName: 'longOffset',
  })
  if (timeZone) formats.set(timeZone, format)
  return format
}

// partsIn is an instant as a date, a time and an offset from UTC in a time
// zone, the browser's when none is given. An unknown zone throws RangeError.
export function partsIn(at: Date, timeZone?: string): Parts {
  const format = formatIn(timeZone)
  const p: Partial<Record<Intl.DateTimeFormatPartTypes, string>> = {}
  for (const part of format.formatToParts(at)) p[part.type] = part.value
  // "GMT+02:00", or "GMT" itself for UTC
  const offset = (p.timeZoneName ?? 'GMT').replace(/^GMT$/, 'GMT+00:00').replace(/^GMT/, '')
  return { date: `${p.year}-${p.month}-${p.day}`, time: `${p.hour}:${p.minute}:${p.second}`, offset }
}

function nodeTime(at: Date, zone: string | undefined): string | null {
  if (!zone) return null
  try {
    const p = partsIn(at, zone)
    return `${p.date} ${p.time} ${p.offset}`
  } catch {
    return null
  }
}

// Go writes a time it never set as the zero time.
const unset = (at: string) => !at || at.startsWith('0001-01-01T00:00:00')

// Time shows an instant in the browser's time zone with its offset, and its
// date when that is not today; the tooltip gives the node's time and UTC
// (spec-ui 3.1). nodeZone is the session's.
export function Time({ at, nodeZone }: { at: string; nodeZone?: string }) {
  const when = new Date(at)
  if (unset(at) || Number.isNaN(when.getTime())) return <span className="time">-</span>
  const local = partsIn(when)
  const today = partsIn(new Date()).date === local.date
  const node = nodeTime(when, nodeZone)
  const utc = partsIn(when, 'UTC')
  return (
    <Tooltip
      content={
        <>
          {node && (
            <>
              Node ({nodeZone}): {node}
              <br />
            </>
          )}
          UTC: {utc.date} {utc.time}
        </>
      }
    >
      <time className="time num" dateTime={at}>
        {today ? '' : `${local.date} `}
        {local.time} {local.offset}
      </time>
    </Tooltip>
  )
}
