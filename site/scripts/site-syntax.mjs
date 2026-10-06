// Refuses the syntax that only the site reads: GitHub and the package show it
// as the text it is, and a page is written for both. VitePress turns
// ::: tip into a box, [[toc]] into a table of contents and <<< @/file into
// the file. Fenced blocks and code spans are not read, so they can show it.
// It needs nothing installed.

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { proseLines } from './prose.mjs'

const syntax = [
  [/^\s*(?:>\s*)*:{3,}\s*(?:tip|info|warning|danger|details|raw|code-group)\b/, 'a container'],
  [/^\s*\[\[toc\]\]\s*$/, 'a table of contents'],
  [/^\s*<<<\s/, 'a file include'],
]

// siteOnly lists the lines of a page that use it.
export function siteOnly(text) {
  const found = []
  for (const { number, line } of proseLines(text)) {
    for (const [pattern, what] of syntax) {
      if (pattern.test(line)) found.push({ line: number, text: line.trim(), what })
    }
  }
  return found
}

function pagesOf(dir, prefix = '') {
  const out = []
  for (const entry of readdirSync(join(dir, prefix), { withFileTypes: true })) {
    if (entry.name.startsWith('.')) continue
    if (entry.isDirectory()) out.push(...pagesOf(dir, `${prefix}${entry.name}/`))
    else if (entry.name.endsWith('.md')) out.push(prefix + entry.name)
  }
  return out.sort()
}

// check returns a line for each use of it in the pages of a directory.
export function check(docs) {
  const problems = []
  for (const page of pagesOf(docs)) {
    for (const { line, what } of siteOnly(readFileSync(join(docs, page), 'utf8'))) {
      problems.push(`${page}:${line} has ${what}, which the site renders and GitHub shows as text`)
    }
  }
  return problems
}

function main() {
  const docs = fileURLToPath(new URL('../../docs/', import.meta.url))
  if (!existsSync(docs)) {
    console.error(`${docs} is not there`)
    process.exit(1)
  }
  const problems = check(docs)
  for (const problem of problems) console.error(problem)
  if (problems.length > 0) process.exit(1)
  console.log('no page uses syntax that only the site reads')
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main()
}
