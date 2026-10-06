import { proseLines } from '../../scripts/prose.mjs'

// The marks of the inline Markdown of a heading, taken off so that what is
// left is the words: code spans give up their backticks and keep what is in
// them, links and emphasis keep their text.
function words(heading) {
  return heading
    .split(/(`[^`]+`)/)
    .map((part, i) =>
      i % 2
        ? part.slice(1, -1)
        : part
            .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
            .replace(/(\*{1,3})(\S(?:.*?\S)?)\1/g, '$2')
            .replace(/(?<![\p{L}\p{N}])(_{1,3})(\S(?:.*?\S)?)\1(?![\p{L}\p{N}])/gu, '$2'),
    )
    .join('')
}

// titleOf returns the title of a page, the words of its first heading of level
// 1 outside a fenced block, or undefined when it has none.
export function titleOf(text) {
  for (const { line } of proseLines(text)) {
    const heading = /^# +(.+?)(?:\s+#+)?\s*$/.exec(line)
    if (heading) return words(heading[1])
  }
  return undefined
}

// The text of the sidebar is drawn as HTML.
export function escapeHtml(text) {
  return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}
