import { randomBytes } from 'node:crypto'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { afterEach, beforeEach, describe, expect, test } from 'vitest'

import { budgets, entries, failures, imports, measure } from './size-budget.mjs'

let dir

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), 'pco-size-budget-'))
  mkdirSync(join(dir, 'assets'))
})

afterEach(() => {
  rmSync(dir, { recursive: true, force: true })
})

function write(name, content) {
  writeFileSync(join(dir, name), content)
}

// noise does not compress: its gzip is a little larger than it is.
const noise = randomBytes

const page = [
  '<!doctype html><html><head>',
  '<script type="module" crossorigin src="/assets/index-a1.js"></script>',
  '<link rel="modulepreload" crossorigin href="/assets/vendor-b2.js">',
  '<link rel="stylesheet" crossorigin href="/assets/index-c3.css">',
  '</head><body><div id="root"></div></body></html>',
].join('\n')

describe('entries and imports', () => {
  test('entries are the scripts and the preloads of the page', () => {
    expect(entries(page)).toEqual(['assets/index-a1.js', 'assets/vendor-b2.js'])
  })

  test('imports are the static ones only', () => {
    const code = 'import{a as e}from"./vendor-b2.js";import"./side-d4.js";export{x}from "./re-e5.js";const m=()=>import("./FlowMap-f6.js");'
    expect(imports(code)).toEqual(['./vendor-b2.js', './side-d4.js', './re-e5.js'])
  })
})

describe('measure', () => {
  beforeEach(() => {
    write('index.html', page)
    write('licenses.txt', 'Name: react\n')
    write('assets/index-c3.css', 'body{margin:0}')
    write('assets/index-a1.js', 'import{a}from"./vendor-b2.js";import"./shared-g7.js";const m=()=>import("./FlowMap-f6.js");')
    write('assets/vendor-b2.js', 'export const a=1;')
    write('assets/shared-g7.js', 'export const s=1;')
    write('assets/FlowMap-f6.js', 'import{a}from"./vendor-b2.js";import{x}from"./xyflow-h8.js";export default x;')
    write('assets/xyflow-h8.js', 'export const x=2;')
  })

  test('sorts the chunks into what loads at start and what the map loads', () => {
    const m = measure(dir)
    expect(m.initial.files.sort()).toEqual(['assets/index-a1.js', 'assets/shared-g7.js', 'assets/vendor-b2.js'])
    expect(m.map.files.sort()).toEqual(['assets/FlowMap-f6.js', 'assets/xyflow-h8.js'])
    expect(m.map.lazy).toBe(true)
    expect(m.total.files).toHaveLength(8)
    expect(failures(m)).toEqual([])
  })

  test('fails over the initial budget', () => {
    write('assets/vendor-b2.js', noise(budgets.initial + 1000))
    expect(failures(measure(dir))).toEqual([expect.stringMatching(/^the initial JavaScript is 22\d\.\d kB, over its budget of 220\.0 kB$/)])
  })

  test('fails over the budget of all files', () => {
    write('assets/font.woff2', noise(budgets.total))
    expect(failures(measure(dir))).toEqual([expect.stringMatching(/^all files are 45\d\.\d kB, over their budget of 450\.0 kB$/)])
  })

  test('fails over the map budget, counting what only the map loads', () => {
    write('assets/xyflow-h8.js', noise(budgets.map + 1000))
    expect(failures(measure(dir))).toEqual([expect.stringMatching(/^the flow map chunk is 9\d\.\d kB, over its budget of 90\.0 kB$/)])
  })

  test('fails when the map loads at start', () => {
    write('assets/index-a1.js', 'import{a}from"./vendor-b2.js";import M from"./FlowMap-f6.js";')
    expect(failures(measure(dir))).toContain('the flow map is loaded at start; it must be a lazy chunk')
  })

  test('refuses a chunk that names a file dist does not hold', () => {
    write('assets/shared-g7.js', 'import"./gone-z9.js";')
    expect(() => measure(dir)).toThrow(/gone-z9\.js, which is not in/)
  })

  test('refuses a dist without index.html', () => {
    rmSync(join(dir, 'index.html'))
    expect(() => measure(dir)).toThrow(/has no index\.html/)
  })
})
