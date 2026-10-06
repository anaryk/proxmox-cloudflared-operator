// Checks the images of the pages: every image a page shows is a file in
// docs/images, which holds files only (the package ships docs/ and one
// directory down), each file is shown by a page (a -dark twin through its
// -light one) and is at most 400 KiB, and all of them together at most
// 8 MiB. It needs nothing installed.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { extname, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

export const limits = { file: 400 * 1024, total: 8 * 1024 * 1024 }

const kinds = new Set(['.png', '.jpg', '.jpeg', '.webp'])

// pages lists the Markdown files of docs/ and of its directories one down,
// which is as deep as the package ships.
function pages(docs) {
  const out = []
  for (const entry of readdirSync(docs, { withFileTypes: true })) {
    if (entry.name.startsWith('.')) continue
    if (entry.isFile() && entry.name.endsWith('.md')) out.push(entry.name)
    if (entry.isDirectory()) {
      for (const name of readdirSync(join(docs, entry.name))) {
        if (name.endsWith('.md') && !name.startsWith('.')) out.push(join(entry.name, name))
      }
    }
  }
  return out.sort()
}

// imagesOf finds the Markdown images of a page, ![alt](src) and
// ![alt][label] with its definition, outside fenced blocks and code spans.
export function imagesOf(text) {
  const prose = []
  let fence = null
  for (const line of text.split('\n')) {
    const marker = /^\s*(`{3,}|~{3,})/.exec(line)
    if (fence === null && marker) {
      fence = marker[1]
    } else if (fence !== null && marker && marker[1][0] === fence[0] && marker[1].length >= fence.length) {
      fence = null
    } else if (fence === null) {
      prose.push(line)
    }
  }
  const plain = prose.join('\n').replace(/(`+)[\s\S]*?\1/g, '')
  const labels = new Map()
  for (const [, label, src] of plain.matchAll(/^ {0,3}\[([^\]]+)\]:\s*<?([^\s>]+)>?/gm)) {
    labels.set(label.toLowerCase(), src)
  }
  const found = []
  for (const [, alt, src] of plain.matchAll(/!\[([^\]]*)\]\(\s*<?([^\s)>]+)>?(?:\s+"[^"]*")?\s*\)/g)) {
    found.push({ alt, src })
  }
  for (const [, alt, label] of plain.matchAll(/!\[([^\]]*)\]\[([^\]]*)\]/g)) {
    const src = labels.get((label || alt).toLowerCase())
    if (src) found.push({ alt, src })
  }
  return found
}

// check returns the problems of the images of a docs directory, and what
// the images weigh.
export function check(docs, { file = limits.file, total = limits.total } = {}) {
  const problems = []
  const imagesDir = join(docs, 'images')
  const files = new Map()
  let entries = []
  try {
    entries = readdirSync(imagesDir, { withFileTypes: true })
  } catch {
    // no images yet
  }
  for (const entry of entries) {
    const name = `images/${entry.name}`
    if (entry.isDirectory()) {
      problems.push(`docs/${name}/ is a directory: docs/images holds files only, as the package ships nothing deeper`)
    } else if (entry.name.startsWith('.')) {
      problems.push(`docs/${name} is a dotfile, which the package does not ship`)
    } else if (!kinds.has(extname(entry.name).toLowerCase())) {
      problems.push(`docs/${name} is not a PNG, JPEG or WebP image; diagrams are Mermaid blocks in the pages`)
    } else {
      const size = statSync(join(imagesDir, entry.name)).size
      files.set(name, size)
      if (size > file) {
        problems.push(`docs/${name} has ${kib(size)}, more than the ${kib(file)} an image may have`)
      }
    }
  }

  const used = new Set()
  for (const page of pages(docs)) {
    for (const { alt, src } of imagesOf(readFileSync(join(docs, page), 'utf8'))) {
      if (/^[a-z][a-z0-9+.-]*:|^\/\//i.test(src)) {
        problems.push(`docs/${page} shows ${src}, which is not in docs/images: the package and a reader offline would not have it`)
        continue
      }
      const path = resolve(docs, page, '..', decodeURIComponent(src.replace(/[?#].*$/, '')))
      const name = relative(docs, path).split(sep).join('/')
      if (!files.has(name)) {
        problems.push(`docs/${page} shows ${src}, which is not an image in docs/images`)
        continue
      }
      if (alt.trim() === '') {
        problems.push(`docs/${page} shows ${src} without alt text that says what it shows`)
      }
      used.add(name)
      if (/-light\.[a-z]+$/i.test(name)) used.add(name.replace(/-light(\.[a-z]+)$/i, '-dark$1'))
    }
  }
  for (const name of files.keys()) {
    if (!used.has(name)) problems.push(`docs/${name} is shown by no page`)
  }

  const weight = [...files.values()].reduce((sum, size) => sum + size, 0)
  if (weight > total) {
    problems.push(`docs/images has ${kib(weight)}, more than the ${kib(total)} the images may have together`)
  }
  return { problems, count: files.size, weight }
}

function kib(bytes) {
  return `${Math.ceil(bytes / 1024)} KiB`
}

function main() {
  const docs = process.argv[2] ?? fileURLToPath(new URL('../../docs/', import.meta.url))
  const { problems, count, weight } = check(docs)
  for (const problem of problems) console.error(problem)
  if (problems.length > 0) process.exit(1)
  console.log(`${count} images in docs/images, ${kib(weight)} of ${kib(limits.total)}, none above ${kib(limits.file)}`)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main()
}
