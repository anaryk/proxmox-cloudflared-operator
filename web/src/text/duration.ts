// Durations as Go writes them ("10s", "1m0s", "1h2m3.5s", "250ms"), which is
// how the settings and the daemon's answers carry them.

const units: Readonly<Record<string, number>> = {
  ns: 1e-6,
  us: 1e-3,
  µs: 1e-3,
  μs: 1e-3,
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
}

// The longest duration Go holds, 2^63 - 1 nanoseconds, as it writes it.
export const longestDuration = '2562047h47m16.854775807s'
const longestMs = 9_223_372_036_854.775

// parseDuration is a Go duration in milliseconds, or undefined for text that
// is not one, as time.ParseDuration reads it: a sign, then 0 or numbers with
// their units, at most the longest duration Go holds.
export function parseDuration(text: string): number | undefined {
  const m = /^([-+]?)((?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))+$/.exec(text)
  if (/^[-+]?0$/.test(text)) return 0
  if (!m) return undefined
  let total = 0
  for (const [, value, unit] of text.matchAll(/(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g)) {
    total += Number(value) * (units[unit ?? ''] ?? 0)
  }
  if (total > longestMs) return undefined
  return m[1] === '-' ? -total : total
}

// goDuration writes a duration of whole seconds as Go does, "12s", "1m30s",
// "2h0m0s", from nanoseconds, which the daemon's answers carry a duration
// as; it is rounded to the second first, halves away from zero, as the
// daemon rounds what it prints.
export function goDuration(ns: number): string {
  const s = Math.sign(ns) * Math.floor(Math.abs(ns) / 1e9 + 0.5)
  const total = Math.abs(s)
  const sign = s < 0 ? '-' : ''
  if (total < 60) return `${sign}${total}s`
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const rest = total % 60
  return h > 0 ? `${sign}${h}h${m}m${rest}s` : `${sign}${m}m${rest}s`
}

// nanosToMs is a duration of the daemon's answers, in nanoseconds, in the
// milliseconds the page counts in.
export const nanosToMs = (ns: number): number => ns / 1e6

// durationText says a length of time in the words the page uses: "40 s",
// "4 min", "2 h 5 min".
export function durationText(ms: number): string {
  const s = Math.max(Math.round(ms / 1000), 0)
  if (s < 60) return `${s} s`
  const min = Math.floor(s / 60)
  if (min < 60) return `${min} min`
  const h = Math.floor(min / 60)
  return min % 60 === 0 ? `${h} h` : `${h} h ${min % 60} min`
}
