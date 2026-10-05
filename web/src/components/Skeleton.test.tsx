import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { Skeleton } from './Skeleton'

test('lines that hold the place, and a word for screen readers', () => {
  const { container } = render(<Skeleton lines={4} label="Loading the routes" />)
  expect(container.querySelectorAll('.skeleton-line[aria-hidden="true"]')).toHaveLength(4)
  expect(screen.getByRole('status').textContent).toBe('Loading the routes')
})
