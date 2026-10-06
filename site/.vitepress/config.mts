import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import type { Plugin } from 'vite'
import { defineConfig, type DefaultTheme } from 'vitepress'

import { themedImages } from './markdown/images.ts'
import { mermaidFences } from './markdown/mermaid.ts'
import { slugify } from './markdown/slug.mjs'

const repository = 'https://github.com/anaryk/proxmox-cloudflared-operator'
const docs = fileURLToPath(new URL('../../docs/', import.meta.url))

// The sections of the navigation, in order, and what each holds: a page of
// docs/ by its file name, a heading of a page, or a directory, whose pages
// follow in the order of their titles, under its index.md when it has one.
// Only what exists is listed, so a planned page appears once it is written.
// A page in docs/ that no section names stops the build.
type Entry = string | { page: string; heading: string; text: string }

const sections: { text: string; entries: Entry[] }[] = [
  { text: 'Start', entries: ['index.md', 'quickstart.md', 'cloudflare-token.md', 'use-cases/'] },
  { text: 'Guides', entries: ['guides/'] },
  {
    text: 'Concepts',
    entries: ['architecture.md', 'annotations.md', 'identity.md', 'profiles.md', 'security.md', 'networking.md'],
  },
  { text: 'Web interface', entries: ['web-ui.md'] },
  { text: 'Operations', entries: ['operations.md', 'troubleshooting.md', 'uninstall.md', 'appliance.md'] },
  {
    text: 'Reference',
    entries: [
      'cli/',
      'settings.md',
      'files.md',
      'problems.md',
      { page: 'annotations.md', heading: 'the-grammar', text: 'Annotation grammar' },
      'api.md',
      'glossary.md',
      'faq.md',
    ],
  },
  { text: 'Project', entries: ['development.md'] },
]

function title(page: string): string {
  let fenced = false
  for (const line of readFileSync(docs + page, 'utf8').split('\n')) {
    if (/^\s*(```|~~~)/.test(line)) fenced = !fenced
    const heading = !fenced && /^# +(.+?)\s*#*\s*$/.exec(line)
    if (heading) return heading[1].replace(/`/g, '')
  }
  throw new Error(`docs/${page} has no title: its first line should be "# " and the title`)
}

function link(page: string): string {
  return '/' + page.replace(/(^|\/)index\.md$/, '$1').replace(/\.md$/, '')
}

function pagesOf(dir: string): string[] {
  if (!existsSync(docs + dir)) return []
  return readdirSync(docs + dir)
    .filter((name) => name.endsWith('.md') && !name.startsWith('.'))
    .map((name) => dir + name)
}

function byTitle(pages: string[]): DefaultTheme.SidebarItem[] {
  return pages
    .map((page) => ({ text: title(page), link: link(page) }))
    .sort((a, b) => a.text.localeCompare(b.text, 'en'))
}

function items(entries: Entry[], listed: Set<string>): DefaultTheme.SidebarItem[] {
  const out: DefaultTheme.SidebarItem[] = []
  for (const entry of entries) {
    if (typeof entry !== 'string') {
      if (existsSync(docs + entry.page)) out.push({ text: entry.text, link: `${link(entry.page)}#${entry.heading}` })
      continue
    }
    if (!entry.endsWith('/')) {
      if (existsSync(docs + entry)) {
        listed.add(entry)
        out.push({ text: title(entry), link: link(entry) })
      }
      continue
    }
    const pages = pagesOf(entry)
    pages.forEach((page) => listed.add(page))
    const index = entry + 'index.md'
    if (!pages.includes(index)) {
      out.push(...byTitle(pages))
      continue
    }
    out.push({
      text: title(index),
      link: link(index),
      collapsed: true,
      items: byTitle(pages.filter((page) => page !== index)),
    })
  }
  return out
}

function navigation(): { sidebar: DefaultTheme.SidebarItem[]; nav: DefaultTheme.NavItem[] } {
  const listed = new Set<string>()
  const sidebar = sections
    .map((section) => ({ text: section.text, items: items(section.entries, listed) }))
    .filter((section) => section.items.length > 0)

  const onDisk = readdirSync(docs, { withFileTypes: true }).flatMap((entry) => {
    if (entry.name.startsWith('.')) return []
    if (entry.isFile()) return entry.name.endsWith('.md') ? [entry.name] : []
    return entry.isDirectory() ? pagesOf(entry.name + '/') : []
  })
  const unlisted = onDisk.filter((page) => !listed.has(page))
  if (unlisted.length > 0) {
    throw new Error(
      `no section of the navigation names ${unlisted.map((page) => 'docs/' + page).join(', ')}: ` +
        'add it to sections in site/.vitepress/config.mts',
    )
  }

  const nav = sidebar
    .filter((section) => section.text !== 'Start' && section.text !== 'Project')
    .map((section) => ({ text: section.text, link: section.items[0].link! }))
  return { sidebar, nav: [{ text: 'Quickstart', link: '/quickstart' }, ...nav] }
}

const { sidebar, nav } = navigation()

// The pages live in docs/, beside site/ and not in it, and a package that a
// compiled page imports, such as vue, would be looked for in docs/ and above,
// where there is no node_modules. It is looked for from here instead.
function packagesOfSite(): Plugin {
  const here = fileURLToPath(import.meta.url)
  return {
    name: 'pco:packages-of-site',
    enforce: 'pre',
    resolveId(id, importer, options) {
      if (!importer?.startsWith(docs) || /^[./\0]/.test(id)) return null
      return this.resolve(id, here, { ...options, skipSelf: true })
    },
  }
}

// The mark of the web interface, in the orange of traffic of each theme.
function mark(tile: string): string {
  const svg =
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 22 22">' +
    `<rect x="1" y="1" width="20" height="20" rx="5" fill="${tile}"/>` +
    '<circle cx="6.5" cy="11" r="2.3" fill="#fff"/>' +
    '<path d="M9.5 11h6.2M13 7.8l3.2 3.2-3.2 3.2" fill="none" stroke="#fff" stroke-width="2.1" ' +
    'stroke-linecap="round" stroke-linejoin="round"/></svg>'
  return 'data:image/svg+xml,' + encodeURIComponent(svg)
}

export default defineConfig({
  lang: 'en-GB',
  title: 'pco',
  description: 'A Cloudflare Tunnel operator for Proxmox VE',
  base: '/proxmox-cloudflared-operator/',
  srcDir: '../docs',
  cleanUrls: true,
  sitemap: { hostname: 'https://anaryk.github.io/proxmox-cloudflared-operator/' },
  head: [['link', { rel: 'icon', type: 'image/svg+xml', href: mark('#ea7317') }]],

  // The pages are written for GitHub and read as plain text in the package:
  // ids as GitHub makes them, and no HTML or attribute syntax of their own,
  // so that a <name> at the start of a line is text, as it is on GitHub,
  // and not a tag that would end the paragraph.
  markdown: {
    html: false,
    anchor: { slugify },
    attrs: false,
    image: { lazyLoad: true },
    config(md) {
      md.use(mermaidFences)
      md.use(themedImages)
    },
  },

  vite: {
    plugins: [packagesOfSite()],
    // Mermaid comes in chunks that a page loads only when it has a diagram of
    // their kind; the largest is the ELK layout, about 1.4 MB.
    build: { chunkSizeWarningLimit: 1600 },
  },

  themeConfig: {
    logo: { light: mark('#ea7317'), dark: mark('#f3862a'), alt: '' },
    nav,
    sidebar,
    outline: { level: [2, 3], label: 'On this page' },
    search: { provider: 'local' },
    socialLinks: [{ icon: 'github', link: repository }],
  },
})
