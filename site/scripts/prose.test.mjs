import assert from 'node:assert/strict'
import { test } from 'node:test'

import { proseLines } from './prose.mjs'

const numbers = (text) => [...proseLines(text)].map(({ number }) => number)

test('the lines outside fenced blocks, with their numbers', () => {
  const text = ['one', '```text', 'two', '```', 'three', '~~~md', 'four', '~~~', 'five'].join('\n')
  assert.deepEqual(numbers(text), [1, 5, 9])
})

test('a fence ends at its own character, as long as it was, with nothing after it', () => {
  assert.deepEqual(numbers(['~~~text', '```cf-tunnel', '```', '~~~', 'out'].join('\n')), [5])
  assert.deepEqual(numbers(['````md', '```', 'in', '```', '````', 'out'].join('\n')), [6])
  assert.deepEqual(numbers(['```text', '``` still in', '```', 'out'].join('\n')), [4])
})

test('a line that starts with a code span is not a fence', () => {
  assert.deepEqual(numbers(['```not a fence``` here', 'two'].join('\n')), [1, 2])
})

test('a fence that is never closed holds the rest of the page', () => {
  assert.deepEqual(numbers(['one', '```text', 'two', 'three'].join('\n')), [1])
})

test('an indented fence, as in a list item, is one too', () => {
  assert.deepEqual(numbers(['- item', '', '  ```text', '  in', '  ```', 'out'].join('\n')), [1, 2, 6])
})
