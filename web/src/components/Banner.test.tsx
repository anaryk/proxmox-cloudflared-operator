import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { Banner } from './Banner'

test('an icon, the text and one action', () => {
  const { container } = render(
    <Banner tone="fail" action={<button type="button">Review</button>}>
      The pco daemon does not answer.
    </Banner>,
  )
  expect(container.querySelector('.banner-fail > svg')).not.toBeNull()
  expect(container.querySelector('.banner-text')?.textContent).toBe('The pco daemon does not answer.')
  expect(screen.getByRole('button', { name: 'Review' })).toBeTruthy()
})

test('without an action', () => {
  const { container } = render(<Banner tone="info">pco was updated; reload to use it.</Banner>)
  expect(container.querySelector('.banner-action')).toBeNull()
})
