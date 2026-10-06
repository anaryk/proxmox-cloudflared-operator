import type { MarkdownRenderer } from 'vitepress'

// VitePress turns a page into a Vue template, and Vue reads a {{ as the start
// of an expression: it fails the build on {{c d}}, shows {{ 1 + 1 }} as 2 and
// drops {{name}}. GitHub shows all three as they are. Fenced blocks are marked
// v-pre by VitePress; in the other places a page has text, each {{ is written
// as two character references, which Vue reads back as the characters.
const rules = ['text', 'code_inline', 'code_block'] as const

export function literalBraces(md: MarkdownRenderer): void {
  for (const name of rules) {
    const rule = md.renderer.rules[name]!
    md.renderer.rules[name] = (tokens, idx, options, env, self) =>
      rule(tokens, idx, options, env, self).replaceAll('{{', '&#123;&#123;')
  }
}
