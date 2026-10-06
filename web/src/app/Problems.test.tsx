import { render } from '@testing-library/react'
import { expect, test, vi } from 'vitest'

import type { State } from '../api/types.gen'
import populated from '../fixtures/populated.json'
import { Problems } from './Problems'

test('every line in the daemon\'s order, the same line twice included', () => {
  const error = vi.spyOn(console, 'error').mockImplementation(() => {})
  const st = { ...(populated as unknown as State), problems: ['b problem', 'a problem', 'b problem'] }
  const { container } = render(<Problems state={st} />)
  expect([...container.querySelectorAll('li')].map((li) => li.textContent)).toEqual(['b problem', 'a problem', 'b problem'])
  // React says so for two items of one key
  expect(error).not.toHaveBeenCalled()
  error.mockRestore()
})
