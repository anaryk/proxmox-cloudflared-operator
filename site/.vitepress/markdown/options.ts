import type { MarkdownOptions } from 'vitepress'

import { themedImages } from './images.ts'
import { literalBraces } from './literal.ts'
import { mermaidFences } from './mermaid.ts'
import { slugify } from './slug.mjs'

// The pages are written for GitHub and read as plain text in the package:
// ids as GitHub makes them, and no HTML or attribute syntax of their own, so
// that a <name> at the start of a line is not a tag on the site, which would
// end the paragraph. markdownlint's MD033 keeps raw HTML out of the pages.
export const markdown: MarkdownOptions = {
  html: false,
  anchor: { slugify },
  attrs: false,
  image: { lazyLoad: true },
  config(md) {
    md.use(literalBraces)
    md.use(mermaidFences)
    md.use(themedImages)
  },
}
