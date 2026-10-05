import { act, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { expect, test, vi } from 'vitest'

import { Dialog } from './Dialog'

function Page({ onClose = () => {} }: { onClose?: () => void }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Confirm
      </button>
      <Dialog
        open={open}
        onClose={() => {
          setOpen(false)
          onClose()
        }}
        title="Confirm what waits"
        footer={
          <button type="button" onClick={() => setOpen(false)}>
            Cancel
          </button>
        }
      >
        <input aria-label="Type the name" />
      </Dialog>
    </>
  )
}

function openIt() {
  const opener = screen.getByRole('button', { name: 'Confirm' })
  opener.focus()
  fireEvent.click(opener)
  const dialog = document.querySelector('dialog')
  if (!dialog) throw new Error('no dialog')
  return { opener, dialog }
}

test('opens as a modal dialog named by its title', () => {
  render(<Page />)
  const { dialog } = openIt()
  expect(dialog.open).toBe(true)
  expect(screen.getByRole('dialog', { name: 'Confirm what waits' })).toBe(dialog)
})

test('the close button gives focus back to the opener', () => {
  const closed = vi.fn()
  render(<Page onClose={closed} />)
  const { opener, dialog } = openIt()
  screen.getByRole('textbox').focus()
  fireEvent.click(screen.getByRole('button', { name: 'Close' }))
  expect(dialog.open).toBe(false)
  expect(closed).toHaveBeenCalledTimes(1)
  expect(document.activeElement).toBe(opener)
})

test('Esc closes it and gives focus back', () => {
  const closed = vi.fn()
  render(<Page onClose={closed} />)
  const { opener, dialog } = openIt()
  const input = screen.getByRole('textbox')
  input.focus()
  fireEvent.keyDown(input, { key: 'Escape' })
  expect(dialog.open).toBe(false)
  expect(closed).toHaveBeenCalledTimes(1)
  expect(document.activeElement).toBe(opener)
})

test('closed by its owner, it gives focus back without calling onClose', () => {
  const closed = vi.fn()
  render(<Page onClose={closed} />)
  const { opener, dialog } = openIt()
  act(() => {
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  })
  expect(dialog.open).toBe(false)
  expect(closed).not.toHaveBeenCalled()
  expect(document.activeElement).toBe(opener)
})

test('nothing of its content is on the page while it is closed', () => {
  render(<Page />)
  expect(screen.queryByRole('textbox', { hidden: true })).toBeNull()
})
