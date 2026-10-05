// The characters that text from guests, other parties' DNS records and
// Cloudflare must not carry onto the page as they are: controls, the
// characters that change the direction of text, and the characters of
// format that show as nothing. The classes are present.Printable's, from
// words.gen.ts.

import { bidiRanges, controlRanges, formatRanges } from '../gen/words.gen'

type Ranges = readonly (readonly [number, number])[]

function within(ranges: Ranges, cp: number): boolean {
  let lo = 0
  let hi = ranges.length - 1
  while (lo <= hi) {
    const mid = (lo + hi) >> 1
    const [first, last] = ranges[mid] ?? [0, -1]
    if (cp < first) hi = mid - 1
    else if (cp > last) lo = mid + 1
    else return true
  }
  return false
}

// isBidi is present.IsBidi: a control of the direction of text, or a
// separator of lines.
export function isBidi(cp: number): boolean {
  return within(bidiRanges, cp)
}

// replaced says whether present.Printable replaces cp, and the page shows a
// marker in its place.
export function replaced(cp: number): boolean {
  return within(controlRanges, cp) || within(formatRanges, cp) || within(bidiRanges, cp)
}

// printable is present.Printable, with each such character a question mark.
export function printable(s: string): string {
  let out = ''
  for (const c of s) {
    out += replaced(c.codePointAt(0) ?? 0) ? '?' : c
  }
  return out
}

// marker is what the page shows in place of such a character.
export function marker(cp: number): string {
  return `⟨U+${cp.toString(16).toUpperCase().padStart(4, '0')}⟩`
}

export interface Segment {
  text: string
  marker?: string
}

// segments splits s into runs of ordinary text and the characters to show
// as markers, one segment each.
export function segments(s: string): Segment[] {
  const out: Segment[] = []
  let run = ''
  for (const c of s) {
    const cp = c.codePointAt(0) ?? 0
    if (!replaced(cp)) {
      run += c
      continue
    }
    if (run) out.push({ text: run })
    run = ''
    out.push({ text: c, marker: marker(cp) })
  }
  if (run) out.push({ text: run })
  return out
}

// marked is s with the markers written in, for places that take plain text
// only, such as a title.
export function marked(s: string): string {
  return segments(s)
    .map((seg) => seg.marker ?? seg.text)
    .join('')
}
