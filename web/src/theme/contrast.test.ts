import { describe, expect, test } from 'vitest'

import css from './tokens.css?raw'

function declarations(body: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const m of body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
    out[m[1] ?? ''] = (m[2] ?? '').trim()
  }
  return out
}

function block(pattern: RegExp): Record<string, string> {
  const m = pattern.exec(css)
  if (!m) throw new Error(`tokens.css has no block ${pattern}`)
  return declarations(m[1] ?? '')
}

const light = block(/^:root\s*\{([^}]*)\}/m)
const darkBySystem = block(/@media \(prefers-color-scheme: dark\)\s*\{\s*:root:not\(\[data-theme="light"\]\)\s*\{([^}]*)\}/)
const darkByChoice = block(/^:root\[data-theme="dark"\]\s*\{([^}]*)\}/m)

function luminance(hex: string): number {
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(hex)
  if (!m) throw new Error(`${hex} is not a colour of six hex digits`)
  const [r, g, b] = m.slice(1).map((h) => {
    const c = parseInt(h, 16) / 255
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  }) as [number, number, number]
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

// The contrast ratio of WCAG 2.
function ratio(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number]
  return (hi + 0.05) / (lo + 0.05)
}

const text = 4.5
const graphic = 3

// The pairs of spec-ui 11.2: the colour, what it sits on, the least ratio,
// and the ratio the spec states for light and dark, at the precision it
// states it.
const pairs: [string, string, number, string, string][] = [
  ['--control-border', '--surface', graphic, '3.50', '4.00'],
  ['--control-border', '--raised', graphic, '3.33', '3.60'],
  ['--control-border', '--bg', graphic, '3.18', '4.40'],
  ['--text', '--surface', text, '16.4', '13.8'],
  ['--text2', '--surface', text, '7.6', '8.6'],
  ['--muted', '--surface', text, '5.0', '5.8'],
  ['--muted', '--raised', text, '4.75', '5.21'],
  ['--accent', '--surface', text, '5.8', '6.2'],
  ['--on-accent', '--accent', text, '5.8', '6.83'],
  ['--traffic', '--surface', graphic, '3.0', '6.6'],
  ['--traffic-text', '--surface', text, '5.59', '7.7'],
  ['--ok', '--surface', text, '5.43', '6.7'],
  ['--warn', '--surface', text, '5.5', '7.5'],
  ['--fail', '--surface', text, '5.4', '5.0'],
  ['--idle', '--surface', text, '5.56', '5.8'],
  ['--edge', '--surface', graphic, '3.07', '3.57'],
  ['--accent', '--accent-weak', text, '5.01', '5.28'],
  ['--ok', '--ok-weak', text, '4.79', '6.06'],
  ['--warn', '--warn-weak', text, '4.90', '6.59'],
  ['--fail', '--fail-weak', text, '4.55', '4.64'],
  ['--idle', '--idle-weak', text, '4.87', '4.94'],
  ['--traffic-text', '--traffic-weak', text, '4.92', '6.55'],
]

// Text that sits on the page itself, outside the cards: titles,
// descriptions, the labels of the navigation.
const onPage: [string, number][] = [
  ['--text', text],
  ['--text2', text],
  ['--muted', text],
  ['--accent', text],
]

describe.each([
  { name: 'light', theme: light, column: 0 },
  { name: 'dark', theme: darkBySystem, column: 1 },
])('$name', ({ theme, column }) => {
  const colour = (token: string) => {
    const value = theme[token]
    if (value === undefined) throw new Error(`no ${token}`)
    return value
  }

  test.each(pairs)('%s on %s', (fg, bg, least, ...stated) => {
    const r = ratio(colour(fg), colour(bg))
    expect(r).toBeGreaterThanOrEqual(least)
    const want = stated[column] ?? ''
    expect(r.toFixed(want.split('.')[1]?.length ?? 0)).toBe(want)
  })

  test.each(onPage)('%s on the page', (fg, least) => {
    expect(ratio(colour(fg), colour('--bg'))).toBeGreaterThanOrEqual(least)
  })
})

test('the dark theme is the same chosen or by the system', () => {
  expect(darkByChoice).toEqual(darkBySystem)
})

test('both themes set the same colours', () => {
  const colours = (theme: Record<string, string>) => Object.keys(theme).filter((k) => /^#|^rgba/.test(theme[k] ?? '')).sort()
  expect(colours(darkBySystem)).toEqual(colours(light))
})
