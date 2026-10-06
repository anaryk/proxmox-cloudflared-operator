// The daemon quotes what it refuses with Go's %q: a string in double quotes,
// a character in single quotes, the quote and the backslash escaped, and a
// character that does not print as its escape. The page writes its own
// sentences of the same refusals the same way.

import { replaced } from './chars'

const named: Readonly<Record<string, string>> = { '\x07': '\\a', '\b': '\\b', '\f': '\\f', '\n': '\\n', '\r': '\\r', '\t': '\\t', '\v': '\\v' }

const hex = (cp: number, digits: number) => cp.toString(16).padStart(digits, '0')

function escaped(c: string, quote: string): string {
  if (c === quote || c === '\\') return `\\${c}`
  const cp = c.codePointAt(0) ?? 0
  if (!replaced(cp)) return c
  if (named[c]) return named[c]
  if (cp < 0x20 || cp === 0x7f) return `\\x${hex(cp, 2)}`
  return cp < 0x10000 ? `\\u${hex(cp, 4)}` : `\\U${hex(cp, 8)}`
}

// goQuote is a string as %q writes it.
export const goQuote = (s: string): string => `"${[...s].map((c) => escaped(c, '"')).join('')}"`

// goQuoteRune is one character as %q writes a rune.
export const goQuoteRune = (c: string): string => `'${escaped(c, "'")}'`
