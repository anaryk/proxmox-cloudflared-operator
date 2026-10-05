import { fireEvent, render, screen } from '@testing-library/react'
import { expect, test, vi } from 'vitest'

import { Pill } from './Pill'

test('a status of the top bar: icon, label and value', () => {
  const { container } = render(<Pill tone="ok" label="Egress" value="on" />)
  expect(container.textContent).toBe('Egresson')
  expect(container.querySelector('.pill-ok svg')).not.toBeNull()
  expect(screen.queryByRole('button')).toBeNull()
})

test('a pill that opens a list is a button', () => {
  const opened = vi.fn()
  render(<Pill tone="warn" label="Stale" value="4 min" onClick={opened} expanded={false} controls="status-list" />)
  const button = screen.getByRole('button', { name: 'Stale 4 min' })
  expect(button.getAttribute('aria-expanded')).toBe('false')
  expect(button.getAttribute('aria-controls')).toBe('status-list')
  fireEvent.click(button)
  expect(opened).toHaveBeenCalledTimes(1)
})
