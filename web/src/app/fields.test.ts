import { expect, test } from 'vitest'

import { stateFields } from '../api/types.gen'
import { fieldHomes, homeless } from './fields'

test('every field of the state has a home on the page', () => {
  expect(homeless()).toEqual([])
})

test('a field the daemon adds has none until it is given one', () => {
  expect(homeless([...stateFields, 'networks'])).toEqual(['networks'])
})

test('no home is kept for a field the state no longer has', () => {
  expect(Object.keys(fieldHomes).filter((k) => !stateFields.includes(k))).toEqual([])
})
