import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'

import { check } from './anchors.mjs'

let dist

function site(files) {
  dist = mkdtempSync(join(tmpdir(), 'pco-anchors-'))
  for (const [path, content] of Object.entries(files)) {
    mkdirSync(join(dist, path, '..'), { recursive: true })
    writeFileSync(join(dist, path), content)
  }
  return dist
}

afterEach(() => rmSync(dist, { recursive: true, force: true }))

const root = '/pco/'

test('links to pages, headings and files that are there', () => {
  const result = check(
    site({
      'index.html':
        '<a href="/pco/security#a-connector-that-is-not-pcos">a</a>' +
        '<a href="/pco/guides/">b</a><a href="#top">c</a><a href="quickstart.html#before-you-start">d</a>' +
        '<a href="/pco/assets/map.png">e</a><a href="https://example.com/#gone">f</a><a href="mailto:a@example.com">g</a>' +
        '<h1 id="top">pco</h1>',
      'security.html': '<h2 id="a-connector-that-is-not-pcos">A connector that is not pco\'s</h2>',
      'quickstart.html': '<h2 id="before-you-start">Before you start</h2>',
      'guides/index.html': '<a href="../security">back</a>',
      'assets/map.png': '',
    }),
    root,
  )
  assert.deepEqual(result, { problems: [], pages: 4 })
})

test('a heading that is not there, and the id VitePress would have made', () => {
  const { problems } = check(
    site({
      'index.html': '<a href="/pco/security#a-connector-that-is-not-pco-s">a</a>',
      'security.html': '<h2 id="a-connector-that-is-not-pcos">A connector that is not pco\'s</h2>',
    }),
    root,
  )
  assert.deepEqual(problems, [
    'index.html links to /pco/security#a-connector-that-is-not-pco-s, and security.html has no heading or element with the id a-connector-that-is-not-pco-s',
  ])
})

test('a page that is not there, reported once however often it is linked', () => {
  const { problems } = check(
    site({
      'index.html': '<a href="/pco/settings">a</a><a class="mobile" href="/pco/settings">a</a><a href="/other/">b</a>',
    }),
    root,
  )
  assert.deepEqual(problems, [
    'index.html links to /pco/settings, which is not in the site',
    'index.html links to /other/, which is outside the site at /pco/',
  ])
})

test('ids and links as HTML escapes and percent-encodes them', () => {
  const { problems } = check(
    site({
      'index.html': '<a href="/pco/faq?x=1&amp;y=2#caf%C3%A9-%E2%80%9Cmenu%E2%80%9D">a</a><a href="#it&#39;s">b</a>',
      'faq.html': '<h2 id="café-“menu”">x</h2>',
    }),
    root,
  )
  assert.deepEqual(problems, [
    "index.html links to #it's, and index.html has no heading or element with the id it's",
  ])
})

test('a name counts only on an anchor, not on a meta tag or a form field', () => {
  const { problems } = check(
    site({
      'index.html':
        '<meta name="viewport" content="width=device-width"><input name="q">' +
        '<a href="#viewport">a</a><a href="#q">b</a><a href="#old">c</a><a href="#new">d</a>' +
        '<a name="old">x</a><a class="mark" name="new">y</a><a id="top"></a>',
    }),
    root,
  )
  assert.deepEqual(problems, [
    'index.html links to #viewport, and index.html has no heading or element with the id viewport',
    'index.html links to #q, and index.html has no heading or element with the id q',
  ])
})
