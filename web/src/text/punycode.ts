// Decoding of Punycode (RFC 3492), so that a hostname with labels in
// xn-- form can be shown in Unicode beside its ASCII form. Browsers turn
// Unicode into Punycode for a URL, but offer nothing for the way back.

const base = 36
const tMin = 1
const tMax = 26
const skew = 38
const damp = 700
const initialBias = 72
const initialN = 0x80
const maxInt = 0x7fffffff

function adapt(delta: number, points: number, first: boolean): number {
  delta = first ? Math.floor(delta / damp) : delta >> 1
  delta += Math.floor(delta / points)
  let k = 0
  while (delta > ((base - tMin) * tMax) >> 1) {
    delta = Math.floor(delta / (base - tMin))
    k += base
  }
  return k + Math.floor(((base - tMin + 1) * delta) / (delta + skew))
}

function digit(c: number): number {
  if (c >= 0x30 && c <= 0x39) return c - 0x30 + 26 // 0-9
  if (c >= 0x41 && c <= 0x5a) return c - 0x41 // A-Z
  if (c >= 0x61 && c <= 0x7a) return c - 0x61 // a-z
  return -1
}

// decode returns the Unicode of a label in Punycode, without its xn--
// prefix, or null when it is not valid Punycode or decodes to something that
// is not text (a surrogate, a code point past U+10FFFF).
export function decode(input: string): string | null {
  const out: number[] = []
  const delimiter = input.lastIndexOf('-')
  for (let j = 0; j < Math.max(delimiter, 0); j++) {
    const c = input.charCodeAt(j)
    if (c >= 0x80) return null
    out.push(c)
  }
  let n = initialN
  let bias = initialBias
  let i = 0
  for (let at = delimiter > 0 ? delimiter + 1 : 0; at < input.length; ) {
    const old = i
    let w = 1
    for (let k = base; ; k += base) {
      if (at >= input.length) return null
      const d = digit(input.charCodeAt(at++))
      if (d < 0 || d > Math.floor((maxInt - i) / w)) return null
      i += d * w
      const t = k <= bias ? tMin : k >= bias + tMax ? tMax : k - bias
      if (d < t) break
      if (w > Math.floor(maxInt / (base - t))) return null
      w *= base - t
    }
    const count = out.length + 1
    bias = adapt(i - old, count, old === 0)
    if (Math.floor(i / count) > maxInt - n) return null
    n += Math.floor(i / count)
    i %= count
    if (n > 0x10ffff || (n >= 0xd800 && n <= 0xdfff)) return null
    out.splice(i++, 0, n)
  }
  return String.fromCodePoint(...out)
}

// toUnicode returns hostname with its labels in Punycode decoded, or null
// when it has none, or one of them does not decode to more than ASCII. Names
// are not case sensitive, so a label is decoded in lower case.
export function toUnicode(hostname: string): string | null {
  const labels = hostname.split('.')
  let found = false
  for (const [at, label] of labels.entries()) {
    if (!/^xn--/i.test(label)) continue
    const decoded = label.length <= 63 ? decode(label.slice(4).toLowerCase()) : null
    if (decoded === null || [...decoded].every((c) => c < '\x80')) return null
    labels[at] = decoded
    found = true
  }
  return found ? labels.join('.') : null
}
