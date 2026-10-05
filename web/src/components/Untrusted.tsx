import { marked, type Segment, segments } from '../text/chars'
import { toUnicode } from '../text/punycode'
import { Badge } from './Badge'

// Untrusted shows text that comes from guests, from the DNS records of
// others or from Cloudflare, and the daemon's messages that quote them
// (spec-ui 9.6). It is text and nothing else; a character that controls the
// terminal, turns the direction of the line or shows as nothing is shown as
// its code point, and the text is isolated from the line around it. A
// hostname reads left to right, and one in Punycode shows its Unicode form
// after the ASCII one, marked, so that a name made to look like another
// shows as what it is.
export interface UntrustedProps {
  text: string
  hostname?: boolean
  // At most this many characters, the rest left out with an ellipsis and
  // the whole text in the title.
  max?: number
  className?: string
}

function cut(parts: Segment[], max: number | undefined): { parts: Segment[]; cut: boolean } {
  if (max === undefined) return { parts, cut: false }
  let total = 0
  for (const s of parts) total += s.marker ? 1 : [...s.text].length
  if (total <= max) return { parts, cut: false }
  const out: Segment[] = []
  let room = Math.max(max - 1, 0)
  for (const s of parts) {
    if (room === 0) break
    if (s.marker) {
      out.push(s)
      room--
      continue
    }
    const chars = [...s.text]
    out.push({ text: chars.slice(0, room).join('') })
    room -= Math.min(chars.length, room)
  }
  return { parts: out, cut: true }
}

function Text({ text, max, ltr, className }: { text: string; max?: number; ltr?: boolean; className?: string }) {
  const shown = cut(segments(text), max)
  return (
    <bdi dir={ltr ? 'ltr' : undefined} className={className} title={shown.cut ? marked(text) : undefined}>
      {shown.parts.map((s, at) =>
        s.marker ? (
          <span key={at} className="cp">
            {s.marker}
          </span>
        ) : (
          s.text
        ),
      )}
      {shown.cut && '…'}
    </bdi>
  )
}

export function Untrusted({ text, hostname, max, className }: UntrustedProps) {
  const unicode = hostname ? toUnicode(text) : null
  if (unicode === null) return <Text text={text} max={max} ltr={hostname} className={className} />
  return (
    <span className="idn">
      <Text text={text} max={max} ltr className={className} />{' '}
      <Badge tone="info">internationalised name</Badge> <Text text={unicode} max={max} ltr className={className} />
    </span>
  )
}
