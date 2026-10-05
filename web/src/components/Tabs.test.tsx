import { fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { expect, test } from 'vitest'

import { Tabs } from './Tabs'

const tabs = [
  { id: 'overview', label: 'Overview' },
  { id: 'diagnosis', label: 'Diagnosis of the current holder' },
  { id: 'timeline', label: 'Timeline', count: 3 },
]

function Detail() {
  const [selected, setSelected] = useState('overview')
  return (
    <Tabs label="Route" tabs={tabs} selected={selected} onSelect={setSelected}>
      <p>{selected}</p>
    </Tabs>
  )
}

test('one tab stop; the selected tab labels the panel', () => {
  render(<Detail />)
  const all = screen.getAllByRole('tab')
  expect(all.map((t) => t.tabIndex)).toEqual([0, -1, -1])
  expect(screen.getByRole('tabpanel', { name: 'Overview' }).textContent).toBe('overview')
})

test('the arrow keys, Home and End select and move focus', () => {
  render(<Detail />)
  const first = screen.getByRole('tab', { name: 'Overview' })
  first.focus()
  fireEvent.keyDown(first, { key: 'ArrowRight' })
  const second = screen.getByRole('tab', { name: 'Diagnosis of the current holder' })
  expect(document.activeElement).toBe(second)
  expect(second.getAttribute('aria-selected')).toBe('true')
  fireEvent.keyDown(second, { key: 'End' })
  expect(document.activeElement?.textContent).toBe('Timeline3')
  fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowRight' })
  expect(document.activeElement).toBe(screen.getByRole('tab', { name: 'Overview' }))
  fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowLeft' })
  expect(screen.getByRole('tabpanel').textContent).toBe('timeline')
})

test('a click selects', () => {
  render(<Detail />)
  fireEvent.click(screen.getByRole('tab', { name: 'Diagnosis of the current holder' }))
  expect(screen.getByRole('tabpanel').textContent).toBe('diagnosis')
})
