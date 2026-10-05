import { fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { expect, test } from 'vitest'

import { Dialog } from './Dialog'
import { Drawer } from './Drawer'

function Page() {
  const [open, setOpen] = useState(false)
  return (
    <>
      <main>
        <button type="button" onClick={() => setOpen(true)}>
          www.example.com
        </button>
        <button type="button">elsewhere</button>
      </main>
      <Drawer open={open} onClose={() => setOpen(false)} title="www.example.com">
        <button type="button">Run diagnosis</button>
      </Drawer>
    </>
  )
}

function openIt() {
  const opener = screen.getByRole('button', { name: 'www.example.com' })
  opener.focus()
  fireEvent.click(opener)
  return opener
}

test('focus goes to its heading when it opens', () => {
  render(<Page />)
  openIt()
  expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'www.example.com' }))
  expect(screen.getByRole('complementary', { name: 'www.example.com' })).toBeTruthy()
})

test('it is not modal: the page stays usable', () => {
  render(<Page />)
  openIt()
  const elsewhere = screen.getByRole('button', { name: 'elsewhere' })
  elsewhere.focus()
  expect(document.activeElement).toBe(elsewhere)
  expect(screen.getByRole('complementary')).toBeTruthy()
})

test('Esc in it closes it and gives focus back to the opener', () => {
  render(<Page />)
  const opener = openIt()
  const run = screen.getByRole('button', { name: 'Run diagnosis' })
  run.focus()
  fireEvent.keyDown(run, { key: 'Escape' })
  expect(screen.queryByRole('complementary')).toBeNull()
  expect(document.activeElement).toBe(opener)
})

test('the close button does the same', () => {
  render(<Page />)
  const opener = openIt()
  fireEvent.click(screen.getByRole('button', { name: 'Close' }))
  expect(screen.queryByRole('complementary')).toBeNull()
  expect(document.activeElement).toBe(opener)
})

test('Esc in a dialog opened from it closes the dialog only', () => {
  function WithDialog() {
    const [drawer, setDrawer] = useState(true)
    const [dialog, setDialog] = useState(false)
    return (
      <Drawer open={drawer} onClose={() => setDrawer(false)} title="www.example.com">
        <button type="button" onClick={() => setDialog(true)}>
          Remove
        </button>
        <Dialog open={dialog} onClose={() => setDialog(false)} title="Remove www.example.com">
          <input aria-label="Type the hostname" />
        </Dialog>
      </Drawer>
    )
  }
  render(<WithDialog />)
  const remove = screen.getByRole('button', { name: 'Remove' })
  remove.focus()
  fireEvent.click(remove)
  const field = screen.getByRole('textbox', { name: 'Type the hostname' })
  field.focus()
  fireEvent.keyDown(field, { key: 'Escape' })
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(screen.getByRole('complementary')).toBeTruthy()
  expect(document.activeElement).toBe(remove)
  fireEvent.keyDown(remove, { key: 'Escape' })
  expect(screen.queryByRole('complementary')).toBeNull()
})

test('Esc on the page behind leaves it open', () => {
  render(<Page />)
  openIt()
  const elsewhere = screen.getByRole('button', { name: 'elsewhere' })
  elsewhere.focus()
  fireEvent.keyDown(elsewhere, { key: 'Escape' })
  expect(screen.getByRole('complementary')).toBeTruthy()
})
