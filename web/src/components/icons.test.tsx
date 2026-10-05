import { render } from '@testing-library/react'
import type { ComponentType } from 'react'
import { describe, expect, test } from 'vitest'

import * as icons from './icons'

// Every icon but the product mark and the one that picks an icon by tone.
const set = Object.entries(icons).filter(([name]) => name.endsWith('Icon') && !['MarkIcon', 'ToneIcon'].includes(name)) as [
  string,
  ComponentType<icons.IconProps>,
][]

test('the sheet of the mockup and the three icons it lacks', () => {
  expect(set).toHaveLength(36)
  expect(set.map(([name]) => name)).toEqual(expect.arrayContaining(['FrozenIcon', 'RejectedIcon', 'RogueIcon']))
})

describe.each(set)('%s', (_, Icon) => {
  test('is decoration without a label', () => {
    const { container } = render(<Icon />)
    const svg = container.querySelector('svg')
    expect(svg?.getAttribute('aria-hidden')).toBe('true')
    expect(svg?.getAttribute('viewBox')).toBe('0 0 16 16')
    expect(svg?.getAttribute('stroke')).toBe('currentColor')
    expect(svg?.getAttribute('stroke-width')).toBe('1.5')
  })

  test('is an image with one', () => {
    const { getByRole } = render(<Icon label="state" />)
    expect(getByRole('img', { name: 'state' })).toBeTruthy()
  })
})
