import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { type Column, Table } from './Table'

interface Route {
  hostname: string
  owner: string
}

const routes: Route[] = Array.from({ length: 5000 }, (_, i) => ({
  hostname: `host${String(i).padStart(4, '0')}.example.com`,
  owner: `qemu/${100 + (i % 7)}`,
}))

const columns: Column<Route>[] = [
  { key: 'hostname', header: 'Hostname', cell: (r) => r.hostname, sort: (a, b) => a.hostname.localeCompare(b.hostname), lead: true },
  { key: 'owner', header: 'Owner', cell: (r) => r.owner, sort: (a, b) => a.owner.localeCompare(b.owner) },
]

// 680 px at 34 px a row: 20 rows in view.
function show(onActivate?: (r: Route) => void) {
  return render(<Table label="Routes" columns={columns} rows={routes} rowKey={(r) => r.hostname} height={680} onActivate={onActivate} />)
}

const bodyRows = () => screen.getAllByRole('row').filter((r) => r.closest('tbody') && r.getAttribute('aria-hidden') !== 'true')
const indexes = () => bodyRows().map((r) => Number(r.getAttribute('aria-rowindex')))

// What a browser does with a key on a button, which happy-dom does not:
// Enter and Space press it, unless the keydown was prevented.
function press(button: HTMLElement, key: string) {
  if (fireEvent.keyDown(button, { key }) && (key === 'Enter' || key === ' ')) fireEvent.click(button)
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('Table', () => {
  test('30 rows of 5000 in the DOM: those in view and 10 below', () => {
    show()
    expect(bodyRows()).toHaveLength(30)
    expect(screen.getByRole('table', { name: 'Routes' }).getAttribute('aria-rowcount')).toBe('5001')
    expect(indexes()[0]).toBe(2)
    expect(indexes().at(-1)).toBe(31)
  })

  test('one tab stop', () => {
    show()
    expect(bodyRows().filter((r) => r.tabIndex === 0)).toHaveLength(1)
    expect(bodyRows()[0]?.tabIndex).toBe(0)
  })

  test('the arrow keys move between rows in the order they are shown', () => {
    show()
    const first = bodyRows()[0]
    if (!first) throw new Error('no rows')
    first.focus()
    fireEvent.keyDown(first, { key: 'ArrowDown' })
    expect(document.activeElement?.getAttribute('aria-rowindex')).toBe('3')
    fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowUp' })
    fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowUp' })
    expect(document.activeElement?.getAttribute('aria-rowindex')).toBe('2')
    expect(bodyRows().filter((r) => r.tabIndex === 0)).toEqual([document.activeElement])
  })

  test('End brings the last row into the DOM and focuses it; Home the first', () => {
    show()
    const first = bodyRows()[0]
    first?.focus()
    fireEvent.keyDown(first as Element, { key: 'End' })
    expect(document.activeElement?.getAttribute('aria-rowindex')).toBe('5001')
    expect(document.activeElement?.textContent).toBe('host4999.example.comqemu/101')
    const shown = indexes()
    expect(shown).toHaveLength(30)
    expect(shown).toEqual(shown.map((_, i) => (shown[0] ?? 0) + i))
    fireEvent.keyDown(document.activeElement as Element, { key: 'Home' })
    expect(document.activeElement?.getAttribute('aria-rowindex')).toBe('2')
  })

  test('Page Down moves by what is in view', () => {
    show()
    const first = bodyRows()[0]
    first?.focus()
    fireEvent.keyDown(first as Element, { key: 'PageDown' })
    expect(document.activeElement?.getAttribute('aria-rowindex')).toBe('21')
  })

  test('scrolling replaces the rows', () => {
    show()
    const scroller = document.querySelector('.table-scroll') as HTMLElement
    scroller.scrollTop = 34 * 2500
    fireEvent.scroll(scroller)
    expect(indexes()[0]).toBe(2490 + 2)
    expect(bodyRows()).toHaveLength(40)
  })

  test('Enter and a click open a row, on it or on the text of a cell', () => {
    const opened = vi.fn()
    show(opened)
    const second = bodyRows()[1] as HTMLElement
    fireEvent.click(second)
    fireEvent.click(within(second).getByText('qemu/101'))
    second.focus()
    fireEvent.keyDown(second, { key: 'Enter' })
    expect(opened).toHaveBeenCalledTimes(3)
    expect(opened).toHaveBeenCalledWith(routes[1])
  })

  test('a button in a cell is pressed by Enter, Space and a click, and the row is not opened', () => {
    const opened = vi.fn()
    const approved = vi.fn()
    const withButton: Column<Route>[] = [
      ...columns,
      {
        key: 'approve',
        header: 'Approval',
        cell: (r) => (
          <button type="button" onClick={() => approved(r.hostname)}>
            Approve
          </button>
        ),
      },
    ]
    render(<Table label="Routes" columns={withButton} rows={routes.slice(0, 3)} rowKey={(r) => r.hostname} onActivate={opened} />)
    const button = within(bodyRows()[1] as HTMLElement).getByRole('button', { name: 'Approve' })
    button.focus()
    press(button, 'Enter')
    press(button, ' ')
    fireEvent.click(button)
    expect(approved).toHaveBeenCalledTimes(3)
    expect(approved).toHaveBeenCalledWith('host0001.example.com')
    expect(opened).not.toHaveBeenCalled()
  })

  test('the keys in a field of a cell are the field’s, not moves between rows', () => {
    const opened = vi.fn()
    const withField: Column<Route>[] = [
      ...columns,
      { key: 'note', header: 'Note', cell: (r) => <input aria-label={`Note for ${r.hostname}`} /> },
    ]
    render(<Table label="Routes" columns={withField} rows={routes.slice(0, 3)} rowKey={(r) => r.hostname} onActivate={opened} />)
    const field = screen.getByRole('textbox', { name: 'Note for host0001.example.com' })
    field.focus()
    for (const key of ['ArrowDown', 'ArrowUp', 'PageDown', 'Home', 'End', 'Enter', ' ']) {
      expect(fireEvent.keyDown(field, { key }), key).toBe(true)
    }
    expect(document.activeElement).toBe(field)
    expect(opened).not.toHaveBeenCalled()
  })

  test('a header sorts, and says how', () => {
    render(
      <Table label="Routes" columns={columns} rows={routes} rowKey={(r) => r.hostname} height={680} defaultSort={{ key: 'hostname', direction: 'ascending' }} />,
    )
    const header = screen.getByRole('columnheader', { name: /Hostname/ })
    expect(header.getAttribute('aria-sort')).toBe('ascending')
    fireEvent.click(within(header).getByRole('button'))
    expect(header.getAttribute('aria-sort')).toBe('descending')
    expect(bodyRows()[0]?.textContent).toBe('host4999.example.comqemu/101')
    const owner = screen.getByRole('columnheader', { name: /Owner/ })
    fireEvent.click(within(owner).getByRole('button'))
    expect(owner.getAttribute('aria-sort')).toBe('ascending')
    expect(header.hasAttribute('aria-sort')).toBe(false)
  })

  test('the focused row stays focused across a sort', () => {
    render(<Table label="Routes" columns={columns} rows={routes.slice(0, 5)} rowKey={(r) => r.hostname} />)
    const third = bodyRows()[2] as HTMLElement
    third.focus()
    fireEvent.click(within(screen.getByRole('columnheader', { name: /Hostname/ })).getByRole('button'))
    fireEvent.click(within(screen.getByRole('columnheader', { name: /Hostname/ })).getByRole('button'))
    expect(bodyRows().find((r) => r.tabIndex === 0)?.textContent).toBe('host0002.example.comqemu/102')
  })

  test('no rows: what to do instead', () => {
    render(<Table label="Routes" columns={columns} rows={[]} rowKey={(r) => r.hostname} empty="No guest carries the tag pco" />)
    expect(screen.getByText('No guest carries the tag pco')).toBeTruthy()
  })

  test('each cell carries its label for the cards of a phone', () => {
    render(<Table label="Routes" columns={columns} rows={routes.slice(0, 1)} rowKey={(r) => r.hostname} />)
    const cells = within(bodyRows()[0] as HTMLElement).getAllByRole('cell')
    expect(cells.map((c) => c.getAttribute('data-label'))).toEqual(['Hostname', 'Owner'])
    expect(cells[0]?.classList.contains('lead')).toBe(true)
  })

  // A table whose parts are shown as blocks loses its roles in Safari, and
  // VoiceOver reads the cards as plain text; roles set on the elements stay.
  test('the parts of the table name their roles, for the cards of a phone', () => {
    const { container } = render(<Table label="Routes" columns={columns} rows={routes.slice(0, 1)} rowKey={(r) => r.hostname} />)
    const roles = (selector: string) => [...container.querySelectorAll(selector)].map((el) => el.getAttribute('role'))
    expect(roles('table')).toEqual(['table'])
    expect(roles('tr')).toEqual(['row', 'row'])
    expect(roles('th')).toEqual(['columnheader', 'columnheader'])
    expect(roles('td')).toEqual(['cell', 'cell'])
  })

  test('the width of a phone is listened for once, not at each render', () => {
    const listen = vi.spyOn(Object.getPrototypeOf(window.matchMedia('(max-width: 719px)')), 'addEventListener')
    show()
    bodyRows()[0]?.focus()
    for (let i = 0; i < 3; i++) fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowDown' })
    expect(document.activeElement?.getAttribute('aria-rowindex')).toBe('5')
    expect(listen.mock.calls.filter(([type]) => type === 'change')).toHaveLength(1)
  })
})
