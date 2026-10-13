// Checks that every page of the command reference, docs/cli, is linked from
// a page outside it: the reference is written from the help of each command,
// and a command that no guide, concept or operations page points to is one a
// reader finds only by browsing the list. A page that has no place in the
// prose is named in exempt, with the reason. The other two ways a page or an
// image can be lost are checked elsewhere: a page that no section of the
// navigation names stops the build (site/.vitepress/config.mts), and an image
// that no page shows fails image-budget.mjs. It needs nothing installed.

import { readdirSync, readFileSync } from 'node:fs'
import { join, posix } from 'node:path'
import { fileURLToPath } from 'node:url'

import { proseLines } from './prose.mjs'

// The pages of docs/cli that need no link from outside, and why. A group
// does nothing but list its commands, which the pages link one by one.
const group = 'a group of commands, each linked on its own'

export const exempt = new Map(
  [
    'pco-appliance.md',
    'pco-appliance-managed.md',
    'pco-appliance-network.md',
    'pco-claims.md',
    'pco-credential.md',
    'pco-egress.md',
    'pco-guest.md',
    'pco-managed.md',
    'pco-net.md',
    'pco-network.md',
    'pco-network-managed.md',
    'pco-route.md',
    'pco-route-manual.md',
    'pco-segment.md',
    'pco-settings.md',
    'pco-tunnel.md',
  ].map((page) => [page, group]),
)

function pages(docs) {
  const out = []
  for (const entry of readdirSync(docs, { withFileTypes: true })) {
    if (entry.name.startsWith('.')) continue
    if (entry.isFile() && entry.name.endsWith('.md')) out.push(entry.name)
    if (entry.isDirectory()) {
      for (const name of readdirSync(join(docs, entry.name))) {
        if (name.endsWith('.md') && !name.startsWith('.')) out.push(`${entry.name}/${name}`)
      }
    }
  }
  return out.sort()
}

// linksOf finds the targets of the Markdown links of a page, [text](target)
// and the definitions of reference links, outside fenced blocks and code
// spans.
export function linksOf(text) {
  const prose = [...proseLines(text)].map(({ line }) => line).join('\n')
  const plain = prose.replace(/(`+)[\s\S]*?\1/g, '')
  const found = []
  for (const [, target] of plain.matchAll(/\]\(\s*<?([^\s)>]+)>?(?:\s+"[^"]*")?\s*\)/g)) found.push(target)
  for (const [, target] of plain.matchAll(/^ {0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?/gm)) found.push(target)
  return found
}

// check returns the problems of the command reference of a docs directory.
export function check(docs, allowed = exempt) {
  const all = pages(docs)
  const reference = all.filter((page) => page.startsWith('cli/') && page !== 'cli/index.md')
  const linked = new Set()
  for (const page of all) {
    if (page.startsWith('cli/')) continue
    for (const target of linksOf(readFileSync(join(docs, page), 'utf8'))) {
      if (/^[a-z][a-z0-9+.-]*:|^\/\/|^#/i.test(target)) continue
      const path = posix.normalize(posix.join(posix.dirname(page), decodeURIComponent(target.replace(/[?#].*$/, ''))))
      linked.add(path)
    }
  }

  const problems = []
  for (const page of reference) {
    const name = page.slice('cli/'.length)
    if (allowed.has(name)) {
      if (linked.has(page)) problems.push(`docs/${page} is linked from a page now: take it off the exempt list of cli-links.mjs`)
      continue
    }
    if (!linked.has(page)) {
      problems.push(`docs/${page} is linked from no page outside docs/cli: link it where the command is used, or exempt it with a reason`)
    }
  }
  for (const name of allowed.keys()) {
    if (!reference.includes(`cli/${name}`)) problems.push(`cli-links.mjs exempts docs/cli/${name}, which does not exist`)
  }
  return { problems, count: reference.length }
}

function main() {
  const docs = process.argv[2] ?? fileURLToPath(new URL('../../docs/', import.meta.url))
  const { problems, count } = check(docs)
  for (const problem of problems) console.error(problem)
  if (problems.length > 0) process.exit(1)
  console.log(`${count} pages of the command reference, each linked from a page outside it or exempt`)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main()
}
