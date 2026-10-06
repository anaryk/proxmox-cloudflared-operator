import assert from 'node:assert/strict'
import { test } from 'node:test'

import { escapeHtml, titleOf } from './title.mjs'

test('the title is the first heading of level 1', () => {
  assert.equal(titleOf('# Quickstart\n\n## Before you start\n'), 'Quickstart')
  assert.equal(titleOf('Text first.\n\n# Later\n'), 'Later')
  assert.equal(titleOf('# Closed #\n'), 'Closed')
  assert.equal(titleOf('# Closed ##   \n'), 'Closed')
  assert.equal(titleOf('# C#\n'), 'C#')
  assert.equal(titleOf('No heading\n\n## Only two\n'), undefined)
})

test('a line in a fenced block is no heading', () => {
  assert.equal(titleOf('```sh\n# a comment\n```\n\n# The title\n'), 'The title')
  assert.equal(titleOf('~~~text\n```cf-tunnel\n# a comment\n~~~\n\n# The title\n'), 'The title')
})

test('the title is the text of the heading, without its marks', () => {
  assert.equal(titleOf('# The `pco apply` command\n'), 'The pco apply command')
  assert.equal(titleOf('# Adding `<host>` routes\n'), 'Adding <host> routes')
  assert.equal(titleOf('# Files like `*.md` and `*.txt`\n'), 'Files like *.md and *.txt')
  assert.equal(titleOf('# A **bold** and an *emphasised* word\n'), 'A bold and an emphasised word')
  assert.equal(titleOf('# An _emphasised_ word and gate_tag_name\n'), 'An emphasised word and gate_tag_name')
  assert.equal(titleOf('# See the [security page](security.md)\n'), 'See the security page')
})

test('the characters of HTML are escaped for the sidebar, which draws its text as HTML', () => {
  assert.equal(escapeHtml('Adding <host> & <port> routes'), 'Adding &lt;host&gt; &amp; &lt;port&gt; routes')
  assert.equal(escapeHtml('Plain'), 'Plain')
})
