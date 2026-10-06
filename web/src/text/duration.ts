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

// parseDuration is a Go duration in milliseconds, or undefined for text that
// is not one.
export function parseDuration(text: string): number | undefined {
  const m = /^([-+]?)((?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))+$/.exec(text)
  if (text === '0') return 0
  if (!m) return undefined
  let total = 0
  for (const [, value, unit] of text.matchAll(/(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g)) {
    total += Number(value) * (units[unit ?? ''] ?? 0)
  }
  return m[1] === '-' ? -total : total
}

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
