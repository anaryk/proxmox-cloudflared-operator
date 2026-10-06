import type { MarkdownRenderer } from 'vitepress'

// A fence of the language mermaid becomes the component that draws it in the
// browser. The source travels URI-encoded in an attribute, so that neither
// the {{ nor the < of a diagram ever reaches the Vue compiler, which reads
// every page as a template.
export function mermaidFences(md: MarkdownRenderer): void {
  const fence = md.renderer.rules.fence!
  md.renderer.rules.fence = (tokens, idx, options, env, self) => {
    const token = tokens[idx]
    if (token.info.trim().split(/\s+/, 1)[0] !== 'mermaid') {
      return fence(tokens, idx, options, env, self)
    }
    return `<MermaidDiagram code="${encodeURIComponent(token.content)}" />\n`
  }
}
