import { fireEvent, render, screen } from '@testing-library/react'
import { expect, test, vi } from 'vitest'

import { Button, IconButton } from './Button'
import { RefreshIcon } from './icons'

test('a button that does not submit unless asked to', () => {
  const clicked = vi.fn()
  render(
    <Button variant="primary" icon={<RefreshIcon />} onClick={clicked}>
      Sync now
    </Button>,
  )
  const button = screen.getByRole('button', { name: 'Sync now' })
  expect(button.getAttribute('type')).toBe('button')
  expect(button.className).toBe('btn btn-primary')
  fireEvent.click(button)
  expect(clicked).toHaveBeenCalledTimes(1)
})

test('a refused action says why and does nothing', () => {
  const clicked = vi.fn()
  render(
    <Button type="submit" onClick={clicked} disabledReason="needs Sys.Modify on /">
      Approve
    </Button>,
  )
  const button = screen.getByRole('button', { name: 'Approve' })
  expect(button.getAttribute('aria-disabled')).toBe('true')
  expect(button.getAttribute('type')).toBe('button')
  expect(screen.getByText('needs Sys.Modify on /').id).toBe(button.getAttribute('aria-describedby'))
  fireEvent.click(button)
  expect(clicked).not.toHaveBeenCalled()
})

test('an icon button is named by its label', () => {
  render(<IconButton label="Close" icon={<RefreshIcon />} />)
  expect(screen.getByRole('button', { name: 'Close' }).getAttribute('title')).toBe('Close')
})
