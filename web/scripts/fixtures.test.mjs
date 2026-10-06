import { readdirSync, readFileSync } from 'node:fs'

import { describe, expect, test } from 'vitest'

import { fixtures, fixturesDir, largeState, text } from './fixtures.mjs'

describe('the fixtures are what the goldens make now', () => {
  const made = fixtures()

  test('no file more or less', () => {
    const files = readdirSync(fixturesDir).filter((f) => f.endsWith('.json'))
    expect(files.sort()).toEqual(Object.keys(made).map((n) => `${n}.json`).sort())
  })

  test.each(Object.keys(made))('%s', (name) => {
    expect(readFileSync(`${fixturesDir}${name}.json`, 'utf8'), 'run npm run fixtures').toBe(text(made[name]))
  })
})

test('a derived state keeps every field of its golden', () => {
  const { populated, untagged, rogue } = fixtures()
  const fields = (o) => Object.keys(JSON.parse(JSON.stringify(o))).sort()
  for (const st of [rogue]) expect(fields(st)).toEqual(fields(populated))
  // a state without problems has no hold and no offer, as the daemon writes it
  expect(fields(untagged)).toEqual(fields(populated).filter((f) => f !== 'hold' && f !== 'offer'))
})

test('the large state has as many routes as asked, one per hostname', () => {
  const st = largeState(1000)
  expect(st.routes).toHaveLength(1000)
  expect(new Set(st.routes.map((r) => r.hostname)).size).toBe(1000)
})
