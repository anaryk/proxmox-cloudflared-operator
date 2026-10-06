import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'

import { check, siteOnly } from './site-syntax.mjs'

let docs

function pages(files) {
  docs = mkdtempSync(join(tmpdir(), 'pco-syntax-'))
  for (const [path, content] of Object.entries(files)) {
    mkdirSync(join(docs, path, '..'), { recursive: true })
    writeFileSync(join(docs, path), content)
  }
  return docs
}

afterEach(() => {
  if (docs) rmSync(docs, { recursive: true, force: true })
  docs = undefined
})

test('a container is refused, whatever its name or its indent', () => {
  for (const line of [
    '::: tip',
    ':::tip',
    '::: warning Watch out',
    ':::: details Why',
    '::: code-group',
    '  ::: danger',
    '> ::: info',
    '- a list item\n\n  ::: tip\n  Text\n  :::',
  ]) {
    assert.equal(siteOnly(`# Page\n\n${line}\n`).length, 1, line)
  }
})

test('the table of contents and a snippet include are refused', () => {
  assert.deepEqual(siteOnly('# Page\n\n[[toc]]\n'), [{ line: 3, text: '[[toc]]', what: 'a table of contents' }])
  assert.deepEqual(siteOnly('# Page\n\n  [[toc]]  \n'), [{ line: 3, text: '[[toc]]', what: 'a table of contents' }])
  assert.deepEqual(siteOnly('# Page\n\n<<< @/snippet.ts{1,2}\n'), [
    { line: 3, text: '<<< @/snippet.ts{1,2}', what: 'a file include' },
  ])
})

test('the same text in a fence, a code span or the middle of a line is shown as it is', () => {
  const text = [
    '# Page',
    '',
    'A container opens with `::: tip` and the table with `[[toc]]`.',
    'A [[toc]] in a sentence, and a ::: in it, and <<< too.',
    '',
    '```text',
    '::: tip',
    '[[toc]]',
    '<<< @/a.ts',
    '```',
    '',
    '~~~md',
    '```',
    '::: tip',
    '~~~',
    '',
    '    ::: output of a command',
    '',
    ':::22 and ::: that name no container',
    '',
  ].join('\n')
  assert.deepEqual(siteOnly(text), [])
})

test('a fence ends at its own marker only', () => {
  const text = ['~~~text', '```cf-tunnel', '~~~', '', '::: tip', ''].join('\n')
  assert.deepEqual(siteOnly(text), [{ line: 5, text: '::: tip', what: 'a container' }])
})

test('the pages of a directory are read, at any depth', () => {
  const root = pages({
    'index.md': '# Home\n',
    'guides/one.md': '# One\n\n::: tip\nText\n:::\n',
    'guides/deeper/two.md': '# Two\n\n[[toc]]\n',
    'images/map.png': '',
  })
  assert.deepEqual(check(root), [
    'guides/deeper/two.md:3 has a table of contents, which the site renders and GitHub shows as text',
    'guides/one.md:3 has a container, which the site renders and GitHub shows as text',
  ])
})

test('a clean directory has nothing to report', () => {
  assert.deepEqual(check(pages({ 'index.md': '# Home\n\nText.\n' })), [])
})
