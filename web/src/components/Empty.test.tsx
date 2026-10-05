import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { Empty } from './Empty'

test('what is missing, what to do, and the action', () => {
  render(
    <Empty title="No guest carries the tag pco" action={<a href="/guests">Open the guests</a>}>
      Add the tag to a guest and name a hostname in its Notes.
    </Empty>,
  )
  expect(screen.getByText('No guest carries the tag pco')).toBeTruthy()
  expect(screen.getByText('Add the tag to a guest and name a hostname in its Notes.')).toBeTruthy()
  expect(screen.getByRole('link', { name: 'Open the guests' })).toBeTruthy()
})
