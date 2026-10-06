import { expect, test } from 'vitest'

import { durationText, longestDuration, parseDuration } from './duration'

test.each([
  ['10s', 10_000],
  ['1m0s', 60_000],
  ['5m0s', 300_000],
  ['1h2m3.5s', 3_723_500],
  ['250ms', 250],
  ['1.5h', 5_400_000],
  ['0s', 0],
  ['0', 0],
  ['-1m', -60_000],
])('parseDuration(%j)', (text, ms) => {
  expect(parseDuration(text)).toBe(ms)
})

test.each(['', '10', 's', '10 s', '1d', 'ten seconds', '1m0'])('parseDuration(%j) is no duration', (text) => {
  expect(parseDuration(text)).toBeUndefined()
})

test.each([
  [0, '0 s'],
  [40_400, '40 s'],
  [240_000, '4 min'],
  [7_200_000, '2 h'],
  [7_500_000, '2 h 5 min'],
])('durationText(%d)', (ms, text) => {
  expect(durationText(ms)).toBe(text)
})

test('as time.ParseDuration: a signed zero, and nothing longer than Go holds', () => {
  expect(parseDuration('+0')).toBe(0)
  expect(parseDuration('-0')).toBe(0)
  expect(parseDuration(longestDuration)).toBeCloseTo(9_223_372_036_854.775, 0)
  expect(parseDuration('2562048h')).toBeUndefined()
  expect(parseDuration('99999999h')).toBeUndefined()
})
