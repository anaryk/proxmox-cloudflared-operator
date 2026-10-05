import { render } from '@testing-library/react'
import { expect, test } from 'vitest'

import { Badge } from './Badge'

test('a word on the weak colour of its tone', () => {
  const { container } = render(<Badge tone="warn">wildcard</Badge>)
  const badge = container.querySelector('span')
  expect(badge?.className).toBe('badge badge-warn')
  expect(badge?.textContent).toBe('wildcard')
})

test('the outline of a level', () => {
  const { container } = render(<Badge outline>port</Badge>)
  expect(container.querySelector('span')?.className).toBe('badge badge-outline')
})
