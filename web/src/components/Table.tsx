import { type KeyboardEvent, type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'

import { rowHeights, usePreferences } from '../theme/theme'

export interface Column<T> {
  key: string
  header: string
  cell: (row: T) => ReactNode
  sort?: (a: T, b: T) => number
  className?: string
  // The first line of the card a row becomes on a phone; the first column
  // when none is.
  lead?: boolean
}

export interface SortOrder {
  key: string
  direction: 'ascending' | 'descending'
}

export interface TableProps<T> {
  label: string
  columns: readonly Column<T>[]
  rows: readonly T[]
  rowKey: (row: T) => string
  defaultSort?: SortOrder
  // A row is opened with Enter, Space or a click.
  onActivate?: (row: T) => void
  // The key of the row whose detail is open.
  current?: string
  // The most the table takes on the page, in pixels; it scrolls past that.
  height?: number
  empty?: ReactNode
}

// Rows above and below those in view that are in the DOM as well.
export const overscan = 10

// Below this width each row is a card: a line per column (base.css).
const cards = '(max-width: 719px)'
const cardLine = 20
const cardPadding = 17
const headHeight = 30

function useCards(): boolean {
  return useSyncExternalStore(
    (changed) => {
      const query = window.matchMedia(cards)
      query.addEventListener('change', changed)
      return () => query.removeEventListener('change', changed)
    },
    () => window.matchMedia(cards).matches,
  )
}

// Table shows many rows, but has only those in view in the DOM, with a margin
// (spec-ui 10). It is one stop of the Tab key; the arrow keys, Page Up, Page
// Down, Home and End move between the rows, which keep the order they are
// shown in. Screen readers are told the number of rows and where each one is.
export function Table<T>({ label, columns, rows, rowKey, defaultSort, onActivate, current, height = 560, empty }: TableProps<T>) {
  const { density } = usePreferences()
  const asCards = useCards()
  const scroller = useRef<HTMLDivElement>(null)
  const [sort, setSort] = useState(defaultSort)
  const [scrollTop, setScrollTop] = useState(0)
  const [viewport, setViewport] = useState(height)
  const [active, setActive] = useState<string>()
  const focusAfter = useRef<number | undefined>(undefined)

  const sorted = useMemo(() => {
    if (!sort) return rows
    const by = columns.find((c) => c.key === sort.key)?.sort
    if (!by) return rows
    const direction = sort.direction === 'ascending' ? 1 : -1
    return [...rows].sort((a, b) => direction * by(a, b))
  }, [rows, columns, sort])

  useEffect(() => {
    const el = scroller.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(() => setViewport(el.clientHeight || height))
    observer.observe(el)
    return () => observer.disconnect()
  }, [height])

  const rowHeight = asCards ? columns.length * cardLine + cardPadding : rowHeights[density]
  const head = asCards ? 0 : headHeight
  const inView = Math.ceil(viewport / rowHeight)
  const top = Math.floor(scrollTop / rowHeight)
  const start = Math.max(0, top - overscan)
  const end = Math.min(sorted.length, top + inView + overscan)
  const activeIndex = active === undefined ? -1 : sorted.findIndex((r) => rowKey(r) === active)
  // The row last moved to, or else the first in view.
  const tabStop = activeIndex >= start && activeIndex < end ? activeIndex : Math.min(top, Math.max(end - 1, 0))

  const rowAt = (index: number) => scroller.current?.querySelector<HTMLElement>(`tr[data-index="${index}"]`)

  useLayoutEffect(() => {
    const index = focusAfter.current
    if (index === undefined) return
    focusAfter.current = undefined
    rowAt(index)?.focus()
  })

  const moveTo = (index: number) => {
    const to = Math.min(Math.max(index, 0), sorted.length - 1)
    const row = sorted[to]
    const el = scroller.current
    if (row === undefined || !el) return
    setActive(rowKey(row))
    let next = scrollTop
    if (to * rowHeight < next) next = to * rowHeight
    else if (head + (to + 1) * rowHeight > next + viewport) next = head + (to + 1) * rowHeight - viewport
    if (next !== scrollTop) {
      el.scrollTop = next
      setScrollTop(next)
    }
    const shown = rowAt(to)
    if (shown) shown.focus()
    else focusAfter.current = to
  }

  const keyDown = (e: KeyboardEvent<HTMLTableRowElement>, index: number, row: T) => {
    const page = Math.max(inView - 1, 1)
    const moves: Record<string, number> = {
      ArrowDown: index + 1,
      ArrowUp: index - 1,
      PageDown: index + page,
      PageUp: index - page,
      Home: 0,
      End: sorted.length - 1,
    }
    if (Object.hasOwn(moves, e.key)) {
      e.preventDefault()
      moveTo(moves[e.key] ?? index)
    } else if ((e.key === 'Enter' || e.key === ' ') && onActivate) {
      e.preventDefault()
      onActivate(row)
    }
  }

  const sortBy = (key: string) =>
    setSort((now) => ({ key, direction: now?.key === key && now.direction === 'ascending' ? 'descending' : 'ascending' }))

  const leadKey = (columns.find((c) => c.lead) ?? columns[0])?.key

  return (
    <div
      ref={scroller}
      className="table-scroll"
      style={{ maxHeight: height }}
      onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
    >
      <table className="table" aria-label={label} aria-rowcount={sorted.length + 1}>
        <thead>
          <tr aria-rowindex={1}>
            {columns.map((c) => {
              const on = sort?.key === c.key
              return (
                <th key={c.key} scope="col" className={c.className} aria-sort={on ? sort.direction : undefined}>
                  {c.sort ? (
                    <button type="button" className="th-sort" onClick={() => sortBy(c.key)}>
                      {c.header}
                      <span className="sort-mark" aria-hidden="true">
                        {on ? (sort.direction === 'ascending' ? '▲' : '▼') : ''}
                      </span>
                    </button>
                  ) : (
                    c.header
                  )}
                </th>
              )
            })}
          </tr>
        </thead>
        <tbody>
          {sorted.length === 0 && (
            <tr>
              <td colSpan={columns.length} className="table-empty">
                {empty ?? 'Nothing to show.'}
              </td>
            </tr>
          )}
          {start > 0 && (
            <tr className="table-spacer" aria-hidden="true">
              <td colSpan={columns.length} style={{ height: start * rowHeight }} />
            </tr>
          )}
          {sorted.slice(start, end).map((row, at) => {
            const index = start + at
            const key = rowKey(row)
            return (
              <tr
                key={key}
                data-index={index}
                aria-rowindex={index + 2}
                aria-current={key === current ? 'true' : undefined}
                tabIndex={index === tabStop ? 0 : -1}
                className={onActivate ? 'table-row table-row-open' : 'table-row'}
                onFocus={() => setActive(key)}
                onClick={() => {
                  setActive(key)
                  onActivate?.(row)
                }}
                onKeyDown={(e) => keyDown(e, index, row)}
              >
                {columns.map((c) => (
                  <td key={c.key} className={[c.className, c.key === leadKey && 'lead'].filter(Boolean).join(' ') || undefined} data-label={c.header}>
                    {c.cell(row)}
                  </td>
                ))}
              </tr>
            )
          })}
          {end < sorted.length && (
            <tr className="table-spacer" aria-hidden="true">
              <td colSpan={columns.length} style={{ height: (sorted.length - end) * rowHeight }} />
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}
