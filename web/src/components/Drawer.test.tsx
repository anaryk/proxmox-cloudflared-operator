import { fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { expect, test } from 'vitest'

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

test('Esc on the page behind leaves it open', () => {
  render(<Page />)
  openIt()
  const elsewhere = screen.getByRole('button', { name: 'elsewhere' })
  elsewhere.focus()
  fireEvent.keyDown(elsewhere, { key: 'Escape' })
  expect(screen.getByRole('complementary')).toBeTruthy()
})
