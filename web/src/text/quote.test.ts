import { expect, test } from 'vitest'

import { goQuote, goQuoteRune } from './quote'

// What fmt.Sprintf("%q", ...) prints for each.
test.each([
  ['a_b', '"a_b"'],
  ['say "hi"', '"say \\"hi\\""'],
  ["it's", '"it\'s"'],
  ['back\\slash', '"back\\\\slash"'],
  ['tab\there', '"tab\\there"'],
  ['bell\x07', '"bell\\a"'],
  ['nul\x00', '"nul\\x00"'],
  ['del\x7f', '"del\\x7f"'],
  ['rlo\u202e', '"rlo\\u202e"'],
  ['bücher', '"bücher"'],
])('a string: %j', (s, quoted) => {
  expect(goQuote(s)).toBe(quoted)
})

test.each([
  ['_', "'_'"],
  ["'", "'\\''"],
  ['"', '\'"\''],
  ['\\', "'\\\\'"],
  ['\n', "'\\n'"],
  ['\u200b', "'\\u200b'"],
  ['ü', "'ü'"],
])('a character: %j', (c, quoted) => {
  expect(goQuoteRune(c)).toBe(quoted)
})
