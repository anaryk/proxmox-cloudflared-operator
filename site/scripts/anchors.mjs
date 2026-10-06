// Checks the links of the built site, site/.vitepress/dist: every link to a
// page of the site leads to a page or file that is there, and every link
// with a fragment to an id on its page. VitePress itself fails the build on
// a link to a page that does not exist, but not on a heading that does not,
// nor on a link of the navigation. It needs nothing installed.

import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

export const base = '/proxmox-cloudflared-operator/'

const entities = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'" }

function unescape(value) {
  return value.replace(/&(#x[0-9a-f]+|#[0-9]+|[a-z]+);/gi, (whole, name) => {
    if (name[0] === '#') {
      const code = name[1] === 'x' || name[1] === 'X' ? parseInt(name.slice(2), 16) : parseInt(name.slice(1), 10)
      return String.fromCodePoint(code)
    }
    return entities[name.toLowerCase()] ?? whole
  })
}

function htmlFiles(dir) {
  const out = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) out.push(...htmlFiles(path))
    else if (entry.name.endsWith('.html')) out.push(path)
  }
  return out
}

// target finds the file a path of the site is served from, as GitHub Pages
// does: the file itself, the page of that name with .html, or the index of
// a directory.
function target(dist, path) {
  const candidates = path === '' || path.endsWith('/') ? [path + 'index.html'] : [path, path + '.html', path + '/index.html']
  for (const candidate of candidates) {
    const file = join(dist, candidate)
    if (existsSync(file) && statSync(file).isFile()) return file
  }
  return null
}

// check returns the broken links of a built site.
export function check(dist, root = base) {
  const pages = htmlFiles(dist)
  const ids = new Map()
  const html = new Map()
  for (const page of pages) {
    const text = readFileSync(page, 'utf8')
    html.set(page, text)
    ids.set(page, new Set([...text.matchAll(/\s(?:id|name)="([^"]*)"/g)].map((m) => unescape(m[1]))))
  }

  // A link the navigation draws twice, for wide and narrow screens, is
  // reported once.
  const problems = new Set()
  for (const page of pages) {
    const name = relative(dist, page).split(sep).join('/')
    const url = new URL(root + name.replace(/(^|\/)index\.html$/, '$1').replace(/\.html$/, ''), 'https://site.invalid')
    for (const [, raw] of html.get(page).matchAll(/<a\s[^>]*?href="([^"]*)"/g)) {
      const href = unescape(raw)
      if (/^[a-z][a-z0-9+.-]*:|^\/\//i.test(href)) continue
      const to = new URL(href, url)
      if (!to.pathname.startsWith(root)) {
        problems.add(`${name} links to ${href}, which is outside the site at ${root}`)
        continue
      }
      const file = target(dist, decodeURIComponent(to.pathname.slice(root.length)))
      if (!file) {
        problems.add(`${name} links to ${href}, which is not in the site`)
        continue
      }
      const fragment = decodeURIComponent(to.hash.slice(1))
      if (fragment === '' || !file.endsWith('.html')) continue
      if (!ids.get(file).has(fragment)) {
        const other = relative(dist, file).split(sep).join('/')
        problems.add(`${name} links to ${href}, and ${other} has no heading or element with the id ${fragment}`)
      }
    }
  }
  return { problems: [...problems], pages: pages.length }
}

function main() {
  const dist = process.argv[2] ?? fileURLToPath(new URL('../.vitepress/dist/', import.meta.url))
  if (!existsSync(join(dist, 'index.html'))) {
    console.error(`${dist} holds no built site: run make docs first`)
    process.exit(1)
  }
  const { problems, pages } = check(dist)
  for (const problem of problems) console.error(problem)
  if (problems.length > 0) process.exit(1)
  console.log(`${pages} pages, every link to the site and its fragment found`)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main()
}
