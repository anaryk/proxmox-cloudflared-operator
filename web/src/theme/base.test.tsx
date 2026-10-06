import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test } from 'vitest'

import { Banner } from '../components/Banner'
import { cardLine, cardPadding, type Column, headHeight, secondLine, Table } from '../components/Table'
import { Tooltip } from '../components/Tooltip'
import base from './base.css?raw'
import { rowHeights } from './theme'
import tokens from './tokens.css?raw'

// happy-dom computes the style of an element from the sheets of its
// document, with the media queries at the width of its window. It lays
// nothing out: this is what the rules say, and what a browser draws from them
// is for the browser suite to check.
const happyDOM = (window as unknown as { happyDOM: { setViewport(size: { width: number; height: number }): void } }).happyDOM
const sheet = document.createElement('style')

beforeEach(() => {
  sheet.textContent = `${tokens}\n${base}`
  document.head.append(sheet)
})

afterEach(() => {
  sheet.remove()
  happyDOM.setViewport({ width: 1024, height: 768 })
  delete document.documentElement.dataset.density
})

// The value of a token in the light theme, the one happy-dom shows.
function token(name: string): string {
  const value = new RegExp(`${name}:\\s*(#[0-9a-f]{6});`).exec(tokens)?.[1]
  if (value === undefined) throw new Error(`tokens.css has no ${name}`)
  return value
}

const style = (el: Element | null | undefined) => {
  if (!el) throw new Error('no element')
  return getComputedStyle(el)
}

interface Route {
  hostname: string
  owner: string
}

const rows: Route[] = [
  { hostname: 'a-name-long-enough-to-take-two-lines-in-a-narrow-column.example.com', owner: 'qemu/101' },
  { hostname: 'www.example.com', owner: 'qemu/102' },
]

const columns: Column<Route>[] = [
  { key: 'hostname', header: 'Hostname', cell: (r) => r.hostname, lead: true },
  { key: 'owner', header: 'Owner', cell: (r) => <span className="muted">{r.owner}</span> },
]

function table() {
  render(<Table label="Routes" columns={columns} rows={rows} rowKey={(r) => r.hostname} current={rows[0]?.hostname} />)
  return screen.getAllByRole('cell')
}

describe('a table on a desktop', () => {
  test('its header and rows are as high as Table.tsx counts them, in both densities', () => {
    const [cell] = table()
    expect(style(document.querySelector('th')).height).toBe(`${headHeight}px`)
    expect(style(cell).height).toBe(`${rowHeights.comfortable}px`)
    document.documentElement.dataset.density = 'compact'
    expect(style(cell).height).toBe(`${rowHeights.compact}px`)
  })

  test('a row of two lines is as high as Table.tsx counts it, in both densities', () => {
    render(<Table label="Routes" columns={columns} rows={rows} rowKey={(r) => r.hostname} lines={2} />)
    const [cell] = screen.getAllByRole('cell')
    expect(style(cell).height).toBe(`calc(${rowHeights.comfortable}px + ${secondLine}px)`)
    document.documentElement.dataset.density = 'compact'
    expect(style(cell).height).toBe(`calc(${rowHeights.compact}px + ${secondLine}px)`)
  })

  test('a long value is cut with an ellipsis, not wrapped onto a second line', () => {
    const cell = style(table()[0])
    expect([cell.whiteSpace, cell.overflow, cell.textOverflow, cell.maxWidth]).toEqual(['nowrap', 'hidden', 'ellipsis', '0'])
  })

  test('muted text in the current row takes the secondary colour', () => {
    table()
    expect(style(screen.getByText('qemu/101')).color).toBe(token('--text2'))
    expect(style(screen.getByText('qemu/102')).color).toBe(token('--muted'))
  })
})

describe('a table on a phone', () => {
  beforeEach(() => {
    happyDOM.setViewport({ width: 400, height: 800 })
  })

  test('a card is as high as Table.tsx counts it', () => {
    const [cell] = table()
    const card = style(cell?.closest('tr'))
    expect(parseFloat(card.paddingTop) + parseFloat(card.paddingBottom) + parseFloat(card.borderBottomWidth)).toBe(cardPadding)
    expect(style(cell).height).toBe(`${cardLine}px`)
  })

  test('a long value ends in an ellipsis', () => {
    const value = style(table()[0]?.querySelector('.cell'))
    expect([value.minWidth, value.whiteSpace, value.overflow, value.textOverflow]).toEqual(['0', 'nowrap', 'hidden', 'ellipsis'])
  })

  test('the lead cell of a row of two lines takes two lines of the card', () => {
    render(<Table label="Routes" columns={columns} rows={rows} rowKey={(r) => r.hostname} lines={2} />)
    const [lead, other] = screen.getAllByRole('cell')
    expect(style(lead).height).toBe(`${2 * cardLine}px`)
    expect(style(other).height).toBe(`${cardLine}px`)
  })
})

test('muted text in a banner takes the secondary colour', () => {
  render(
    <Banner tone="warn">
      Cloudflare is not answering <span className="muted">since 10:42</span>
    </Banner>,
  )
  expect(style(screen.getByText('since 10:42')).color).toBe(token('--text2'))
})

test('a tooltip is fixed to the window, out of the reach of a box that scrolls', () => {
  render(
    <Tooltip content="shared by 2 routes">
      <button type="button">10.0.0.11:8080</button>
    </Tooltip>,
  )
  expect(style(screen.getByRole('tooltip', { hidden: true })).position).toBe('fixed')
})
