// Builds a page of the site's own settings with a {{ in every place Markdown
// has one, and reads the result: GitHub shows each {{ as it is, so the site
// must too. It needs the packages of the site, so make docs-test runs it, not
// make test-scripts.

import assert from 'node:assert/strict'
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { build } from 'vitepress'

const site = fileURLToPath(new URL('..', import.meta.url))
const options = join(site, '.vitepress/markdown/options.ts')

let root
let page

before(async () => {
  const cache = join(site, '.vitepress/cache')
  mkdirSync(cache, { recursive: true })
  root = mkdtempSync(join(cache, 'braces-'))
  mkdirSync(join(root, '.vitepress'))
  writeFileSync(
    join(root, '.vitepress/config.mts'),
    `import { defineConfig } from 'vitepress'\nimport { markdown } from ${JSON.stringify(options)}\nexport default defineConfig({ markdown })\n`,
  )
  copyFileSync(join(site, 'test/fixtures/braces.md'), join(root, 'index.md'))
  await build(root)
  page = readFileSync(join(root, '.vitepress/dist/index.html'), 'utf8')
})

after(() => rmSync(root, { recursive: true, force: true }))

const places = [
  ['prose', 'In prose: {{c d}}, {{name}} and {{ 1 + 1 }}, also ${{ github.ref }}.'],
  [
    'a code span',
    '<code>{{c d}}</code>, <code>{{name}}</code> and <code>{{ 1 + 1 }}</code>, also <code>{{ .Name }}</code>',
  ],
  ['emphasis', '<em>{{c d}}</em>'],
  ['a link', 'title="{{ 1 + 1 }}" target="_blank" rel="noreferrer">{{name}}</a>'],
  ['character references', 'Written as references: {{c d}} and {{ 1 + 1 }}.'],
  ['a table cell', '<td>{{c d}} {{name}} {{ 1 + 1 }}</td>'],
  ['a code span in a table cell', '<td><code>{{c d}}</code> <code>{{name}}</code> <code>{{ 1 + 1 }}</code></td>'],
  ['a list item', '<li>a list item with {{c d}} and <code>{{name}}</code></li>'],
  ['a quote', '<blockquote><p>a quote with {{ 1 + 1 }}</p></blockquote>'],
  ['an indented block', '<pre><code>{{c d}}\n{{name}}\n{{ 1 + 1 }}\n</code></pre>'],
  ['a fenced block', '<span class="line"><span>{{ 1 + 1 }}</span></span>'],
  ['a heading', 'A heading with {{ 1 + 1 }} <a class="header-anchor"'],
]

for (const [place, html] of places) {
  test(`{{ stays as it is in ${place}`, () => {
    assert.ok(page.includes(html), `${html} is not in the page`)
  })
}
