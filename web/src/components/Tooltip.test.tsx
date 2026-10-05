import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import { Tooltip } from './Tooltip'

afterEach(() => {
  vi.restoreAllMocks()
})

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

// The bubble is fixed to the window, so that the box of a table that scrolls
// does not cut it off. happy-dom lays nothing out: the boxes of the child
// (x, y, width, height) are given, the bubble is 120 by 30 px and the window
// 1024 px wide.
test.each([
  ['above its child, centred on it', [200, 100, 80, 20], '64px', '180px'],
  ['below it where there is no room above, as in the first row of a table', [200, 10, 80, 20], '36px', '180px'],
  ['inside the window at its right edge', [990, 100, 30, 20], '64px', '898px'],
  ['inside the window at its left edge', [0, 100, 30, 20], '64px', '6px'],
] as const)('it is placed %s', (_, [x, y, width, height], top, left) => {
  const { trigger, tip } = show()
  vi.spyOn(trigger.parentElement as HTMLElement, 'getBoundingClientRect').mockReturnValue(new DOMRect(x, y, width, height))
  vi.spyOn(tip, 'getBoundingClientRect').mockReturnValue(new DOMRect(0, 0, 120, 30))
  fireEvent.mouseEnter(trigger)
  expect({ top: tip.style.top, left: tip.style.left }).toEqual({ top, left })
})

test('it follows its child when a box around it scrolls', () => {
  const { trigger, tip } = show()
  const child = vi.spyOn(trigger.parentElement as HTMLElement, 'getBoundingClientRect').mockReturnValue(new DOMRect(200, 300, 80, 20))
  vi.spyOn(tip, 'getBoundingClientRect').mockReturnValue(new DOMRect(0, 0, 120, 30))
  fireEvent.mouseEnter(trigger)
  expect(tip.style.top).toBe('264px')
  child.mockReturnValue(new DOMRect(200, 200, 80, 20))
  const scroller = document.createElement('div')
  document.body.append(scroller)
  fireEvent.scroll(scroller)
  expect(tip.style.top).toBe('164px')
  scroller.remove()
})
