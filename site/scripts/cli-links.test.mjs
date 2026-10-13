import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'

import { check, linksOf } from './cli-links.mjs'

let docs

function site(files) {
  docs = mkdtempSync(join(tmpdir(), 'pco-cli-'))
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

test('the links of a page, outside code', () => {
  const text = [
    'See [`pco sync`](cli/pco-sync.md#flags) and [the plan][plan].',
    '',
    '`[not](cli/pco-code.md)`',
    '',
    '```text',
    '[nor](cli/pco-fence.md)',
    '```',
    '',
    '[plan]: cli/pco-plan.md',
  ].join('\n')
  assert.deepEqual(linksOf(text), ['cli/pco-sync.md#flags', 'cli/pco-plan.md'])
})

test('a command linked from a guide, one from the reference only, and an exempt group', () => {
  const { problems, count } = check(
    site({
      'index.md': '# pco\n\n[The commands](cli/index.md)\n',
      'guides/a.md': '# A\n\nRun [pco sync](../cli/pco-sync.md).\n',
      'cli/index.md': '[pco sync](pco-sync.md) [pco plan](pco-plan.md) [pco net](pco-net.md)\n',
      'cli/pco-sync.md': '# pco sync\n',
      'cli/pco-plan.md': '# pco plan\n\nSee [pco sync](pco-sync.md).\n',
      'cli/pco-net.md': '# pco net\n',
    }),
    new Map([['pco-net.md', 'a group']]),
  )
  assert.equal(count, 3)
  assert.deepEqual(problems, [
    'docs/cli/pco-plan.md is linked from no page outside docs/cli: link it where the command is used, or exempt it with a reason',
  ])
})

test('an exemption that is linked after all, and one of a page that is gone', () => {
  const { problems } = check(
    site({
      'operations.md': '# Operations\n\n[pco net](cli/pco-net.md)\n',
      'cli/pco-net.md': '# pco net\n',
    }),
    new Map([
      ['pco-net.md', 'a group'],
      ['pco-gone.md', 'a group'],
    ]),
  )
  assert.deepEqual(problems, [
    'docs/cli/pco-net.md is linked from a page now: take it off the exempt list of cli-links.mjs',
    'cli-links.mjs exempts docs/cli/pco-gone.md, which does not exist',
  ])
})
