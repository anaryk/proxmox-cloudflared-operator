// Measures the interface make ui built into internal/web/ui/dist as the web
// process serves it, each text file gzipped at level 9, and fails over the
// budgets below. The gzip happens in memory, only to measure: what
// is embedded stays uncompressed. Sizes are in kB of 1000 bytes, as Vite
// prints them.

import { readdirSync, readFileSync } from 'node:fs'
import { join, posix } from 'node:path'
import { fileURLToPath } from 'node:url'
import { gzipSync } from 'node:zlib'

export const budgets = {
  initial: 220_000, // the JavaScript a page loads before it can show anything
  total: 450_000, // every file of dist
  map: 90_000, // the lazy chunk of the flow map with what it alone loads
}

// The web process compresses these and serves the rest as they are.
const text = /\.(?:js|css|html|txt|svg|json)$/
// The flow map is src/flow/FlowMap.tsx, loaded with import() by the Overview.
const mapChunk = /^assets\/FlowMap-[\w-]+\.js$/

export function files(dir, prefix = '') {
  return readdirSync(join(dir, prefix), { withFileTypes: true }).flatMap((entry) => {
    const name = prefix ? `${prefix}/${entry.name}` : entry.name
    return entry.isDirectory() ? files(dir, name) : [name]
  })
}

// entries lists the scripts index.html runs and preloads, as paths in dist.
export function entries(html) {
  const found = []
  for (const tag of html.match(/<(?:script|link)\b[^>]*>/gi) ?? []) {
    const script = /^<script/i.test(tag)
    if (!script && !/\brel=["']?modulepreload\b/i.test(tag)) {
      continue
    }
    const ref = tag.match(script ? /\bsrc=["']([^"']+)["']/i : /\bhref=["']([^"']+)["']/i)
    if (ref) {
      found.push(ref[1].replace(/^\//, ''))
    }
  }
  return found
}

// imports lists the chunks a chunk imports statically, as written in it. An
// import() is not one of them: it loads later, when it is called.
export function imports(code) {
  return [...code.matchAll(/\b(?:from|import)\s*["'](\.{1,2}\/[^"']+\.js)["']/g)].map((m) => m[1])
}

// closure is the chunks and every chunk they import statically, directly or not.
export function closure(start, read) {
  const seen = new Set()
  const queue = [...start]
  while (queue.length > 0) {
    const name = queue.pop()
    if (seen.has(name)) {
      continue
    }
    seen.add(name)
    for (const ref of imports(read(name))) {
      queue.push(posix.normalize(posix.join(posix.dirname(name), ref)))
    }
  }
  return seen
}

export function measure(dir) {
  const names = files(dir)
  const size = new Map()
  for (const name of names) {
    const bytes = readFileSync(join(dir, name))
    size.set(name, text.test(name) ? gzipSync(bytes, { level: 9 }).length : bytes.length)
  }
  if (!size.has('index.html')) {
    throw new Error(`${dir} has no index.html`)
  }
  const read = (name) => {
    if (!size.has(name)) {
      throw new Error(`a chunk loads ${name}, which is not in ${dir}`)
    }
    return readFileSync(join(dir, name), 'utf8')
  }
  const sum = (set) => [...set].reduce((n, name) => n + size.get(name), 0)

  const initial = closure(entries(read('index.html')), read)
  const maps = names.filter((name) => mapChunk.test(name))
  if (maps.length > 1) {
    throw new Error(`more than one chunk of the flow map: ${maps.join(', ')}`)
  }
  let map = null
  if (maps.length === 1) {
    const own = [...closure(maps, read)].filter((name) => !initial.has(name))
    map = { files: own, bytes: sum(own), lazy: !initial.has(maps[0]) }
  }
  return {
    initial: { files: [...initial], bytes: sum(initial) },
    total: { files: names, bytes: sum(names) },
    map,
  }
}

export function failures(m) {
  const kB = (n) => `${(n / 1000).toFixed(1)} kB`
  const out = []
  if (m.initial.bytes > budgets.initial) {
    out.push(`the initial JavaScript is ${kB(m.initial.bytes)}, over its budget of ${kB(budgets.initial)}`)
  }
  if (m.total.bytes > budgets.total) {
    out.push(`all files are ${kB(m.total.bytes)}, over their budget of ${kB(budgets.total)}`)
  }
  if (m.map && !m.map.lazy) {
    out.push('the flow map is loaded at start; it must be a lazy chunk')
  }
  if (m.map && m.map.bytes > budgets.map) {
    out.push(`the flow map chunk is ${kB(m.map.bytes)}, over its budget of ${kB(budgets.map)}`)
  }
  return out
}

function main() {
  const dir = fileURLToPath(new URL('../../internal/web/ui/dist/', import.meta.url))
  let m
  try {
    m = measure(dir)
  } catch (err) {
    console.error(err.code === 'ENOENT' ? `${dir} is missing: run make ui first` : err.message)
    process.exit(1)
  }
  const kB = (n) => `${(n / 1000).toFixed(1).padStart(6)} kB`
  console.log(`initial JavaScript ${kB(m.initial.bytes)} of ${kB(budgets.initial)}  ${m.initial.files.join(' ')}`)
  console.log(`all files          ${kB(m.total.bytes)} of ${kB(budgets.total)}  ${m.total.files.length} files`)
  console.log(m.map ? `flow map chunk     ${kB(m.map.bytes)} of ${kB(budgets.map)}  ${m.map.files.join(' ')}` : 'flow map chunk     none yet')
  const over = failures(m)
  for (const line of over) {
    console.error(line)
  }
  if (over.length > 0) {
    process.exit(1)
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main()
}
