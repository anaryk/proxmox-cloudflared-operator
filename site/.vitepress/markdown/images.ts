import { existsSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import type { MarkdownRenderer } from 'vitepress'

// An image whose name ends in -light, as ../images/ui-map-light.png, and that
// has a twin ending in -dark beside it. GitHub and the package show the light
// one, as the page names it; the site puts both into the page and its
// stylesheet shows the one of the theme the reader has.
export function themedImages(md: MarkdownRenderer): void {
  const image = md.renderer.rules.image!
  md.renderer.rules.image = (tokens, idx, options, env, self) => {
    const token = tokens[idx]
    const src = token.attrGet('src') ?? ''
    const named = /^([^?#]+)-light(\.[A-Za-z]+)$/.exec(src)
    const page: string | undefined = env.realPath ?? env.path
    if (!named || !page || /^(?:[a-z][a-z0-9+.-]*:|\/)/i.test(src)) {
      return image(tokens, idx, options, env, self)
    }
    const darkSrc = `${named[1]}-dark${named[2]}`
    if (!existsSync(resolve(dirname(page), decodeURIComponent(darkSrc)))) {
      return image(tokens, idx, options, env, self)
    }
    const dark = Object.assign(Object.create(Object.getPrototypeOf(token)), token)
    dark.attrs = token.attrs!.map(([name, value]) => [name, value])
    dark.attrSet('src', darkSrc)
    dark.attrJoin('class', 'pco-dark-only')
    token.attrJoin('class', 'pco-light-only')
    return image(tokens, idx, options, env, self) + image([dark], 0, options, env, self)
  }
}
