import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'

import { check, imagesOf } from './image-budget.mjs'

let docs

function site(files) {
  docs = mkdtempSync(join(tmpdir(), 'pco-images-'))
  for (const [path, content] of Object.entries(files)) {
    mkdirSync(join(docs, path, '..'), { recursive: true })
    writeFileSync(join(docs, path), typeof content === 'number' ? Buffer.alloc(content) : content)
  }
  return docs
}

afterEach(() => rmSync(docs, { recursive: true, force: true }))

test('a page and the pair of images it shows', () => {
  const result = check(
    site({
      'index.md': '# pco\n\n![The map of routes](images/map-light.png)\n',
      'guides/a.md': '# A\n\n![The map of routes](../images/map-light.png)\n',
      'images/map-light.png': 1000,
      'images/map-dark.png': 1000,
    }),
  )
  assert.deepEqual(result, { problems: [], count: 2, weight: 2000 })
})

test('an image that is too large, and images that are too large together', () => {
  const { problems } = check(
    site({
      'index.md': '![A](images/a.png) ![B](images/b.png)\n',
      'images/a.png': 3000,
      'images/b.png': 2000,
    }),
    { file: 2048, total: 4096 },
  )
  assert.deepEqual(problems, [
    'docs/images/a.png has 3 KiB, more than the 2 KiB an image may have',
    'docs/images has 5 KiB, more than the 4 KiB the images may have together',
  ])
})

test('an image that is missing, one that no page shows, and one without alt text', () => {
  const { problems } = check(
    site({
      'index.md': '![The map](images/gone.png)\n\n![](images/b.png)\n',
      'images/a-dark.png': 10,
      'images/b.png': 10,
    }),
  )
  assert.deepEqual(problems, [
    'docs/index.md shows images/gone.png, which is not an image in docs/images',
    'docs/index.md shows images/b.png without alt text that says what it shows',
    'docs/images/a-dark.png is shown by no page',
  ])
})

test('what the package would not ship, and what is no screenshot', () => {
  const { problems } = check(
    site({
      'index.md': '![Remote](https://example.com/a.png)\n',
      'images/deeper/a.png': 10,
      'images/.hidden.png': 10,
      'images/flow.svg': 10,
    }),
  )
  assert.deepEqual(problems.sort(), [
    'docs/images/.hidden.png is a dotfile, which the package does not ship',
    'docs/images/deeper/ is a directory: docs/images holds files only, as the package ships nothing deeper',
    'docs/images/flow.svg is not a PNG, JPEG or WebP image; diagrams are Mermaid blocks in the pages',
    'docs/index.md shows https://example.com/a.png, which is not in docs/images: the package and a reader offline would not have it',
  ])
})

test('no images at all', () => {
  assert.deepEqual(check(site({ 'index.md': '# pco\n' })), { problems: [], count: 0, weight: 0 })
})

test('images in code are not shown', () => {
  const text = [
    '![Shown](images/a.png)',
    '`![Quoted](images/b.png)`',
    '~~~text',
    '![Fenced](images/c.png)',
    '```',
    '![Still fenced](images/d.png)',
    '~~~',
    '![By label][map]',
    '',
    '[map]: images/e.png',
  ].join('\n')
  assert.deepEqual(imagesOf(text), [
    { alt: 'Shown', src: 'images/a.png' },
    { alt: 'By label', src: 'images/e.png' },
  ])
})
