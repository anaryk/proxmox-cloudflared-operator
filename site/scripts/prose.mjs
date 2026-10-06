// The lines of a page that are not in a fenced block, with their numbers. A
// fence ends at a line of its own character, at least as long as the one that
// opened it, and nothing else on it, so a ``` line inside a ~~~ block is text.
export function* proseLines(text) {
  let fence = null
  let number = 0
  for (const line of text.split('\n')) {
    number++
    const marker = /^\s*(`{3,}|~{3,})(.*)$/.exec(line)
    if (fence === null) {
      if (marker && !(marker[1][0] === '`' && marker[2].includes('`'))) fence = marker[1]
      else yield { number, line }
    } else if (marker && marker[1][0] === fence[0] && marker[1].length >= fence.length && marker[2].trim() === '') {
      fence = null
    }
  }
}
