import { describe, expect, test } from 'vitest'

import { bidiRanges, controlRanges, formatRanges } from '../gen/words.gen'
import { marked, marker, segments } from './chars'

describe('segments', () => {
  test('ordinary text is one segment', () => {
    expect(segments('www.example.com: služba 日本語 🙂')).toEqual([{ text: 'www.example.com: služba 日本語 🙂' }])
  })

  test('each character of the classes is a marker of its own', () => {
    expect(segments('a‮b​​c\u0007')).toEqual([
      { text: 'a' },
      { text: '‮', marker: '⟨U+202E⟩' },
      { text: 'b' },
      { text: '​', marker: '⟨U+200B⟩' },
      { text: '​', marker: '⟨U+200B⟩' },
      { text: 'c' },
      { text: '\u0007', marker: '⟨U+0007⟩' },
    ])
  })

  test('a character outside the first plane is one marker', () => {
    expect(segments('flag\u{e0067}')).toEqual([{ text: 'flag' }, { text: '\u{e0067}', marker: '⟨U+E0067⟩' }])
  })

  test('nothing', () => {
    expect(segments('')).toEqual([])
  })
})

test('marked writes the markers into the text', () => {
  expect(marked('evil‮txt.exe')).toBe('evil⟨U+202E⟩txt.exe')
})

test('marker has four digits at least', () => {
  expect(marker(0x7f)).toBe('⟨U+007F⟩')
})

test.each([
  ['controlRanges', controlRanges],
  ['formatRanges', formatRanges],
  ['bidiRanges', bidiRanges],
])('%s are sorted and apart', (_, ranges) => {
  for (const [at, [lo, hi]] of ranges.entries()) {
    expect(lo).toBeLessThanOrEqual(hi)
    if (at > 0) {
      expect(lo).toBeGreaterThan((ranges[at - 1]?.[1] ?? -1) + 1)
    }
  }
})
