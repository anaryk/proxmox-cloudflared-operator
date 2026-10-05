import { fireEvent, render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { Tooltip } from './Tooltip'

function show() {
  render(
    <Tooltip content="shared by 2 routes">
      <button type="button">10.0.0.11:8080</button>
    </Tooltip>,
  )
  return { trigger: screen.getByRole('button'), tip: screen.getByRole('tooltip', { hidden: true }) }
}

test('describes its child, shown or not', () => {
  const { trigger, tip } = show()
  expect(trigger.getAttribute('aria-describedby')).toBe(tip.id)
  expect(tip.hidden).toBe(true)
})

test('shows on hover and on focus, and goes with Esc', () => {
  const { trigger, tip } = show()
  fireEvent.mouseEnter(trigger)
  expect(tip.hidden).toBe(false)
  fireEvent.mouseLeave(trigger)
  expect(tip.hidden).toBe(true)
  fireEvent.focus(trigger)
  expect(tip.hidden).toBe(false)
  fireEvent.keyDown(document, { key: 'Escape' })
  expect(tip.hidden).toBe(true)
})
