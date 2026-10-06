// The error answers of the daemon and of the web process, and what the page
// does with each code.

export interface ErrorFields {
  error?: string
  code?: string
  field?: string
  missing?: string
  retryAfter?: number
  credential?: unknown
  // and what else the body says, as how to sign in
  [field: string]: unknown
}

// The codes the client makes itself, for a call that got no answer.
export const codeTimeout = 'timeout'
export const codeNetwork = 'network'

export class ApiError extends Error {
  status: number
  code: string
  field?: string
  retryAfter?: number
  missing?: string
  // The credential view of a refused token, with its report.
  credential?: unknown
  // A write that got no answer: it may have happened.
  write: boolean
  // The whole body, as a 401 of /api/session says how to sign in.
  body: unknown

  constructor(status: number, body: ErrorFields, write = false) {
    super(body.error ?? `the answer had status ${status}`)
    this.name = 'ApiError'
    this.status = status
    this.code = body.code ?? 'internal'
    this.field = body.field || undefined
    this.retryAfter = body.retryAfter
    this.missing = body.missing || undefined
    this.credential = body.credential
    this.write = write
    this.body = body
  }
}

export const timeoutWriteText = 'No answer in time: the outcome is unknown. Reload to see the current state.'
export const timeoutReadText = 'No answer in time. Try again, or reload to see the current state.'

// What the page offers next to the message.
export type Remedy = 'look-again' | 'try-again' | 'reload' | 'refresh' | 'sign-in'

export interface Explained {
  text: string
  // The text is the daemon's or the web process's own, which may quote what
  // a guest or Cloudflare wrote: it is shown as untrusted text.
  quoted: boolean
  remedy?: Remedy
}

// The daemon refuses a peer it does not know with these words.
const socketRefused = 'not allowed'

// explain says an error in the page's words, and what to offer with it.
// at is when it happened, for the message that points at the journal.
export function explain(e: ApiError, at: Date = new Date()): Explained {
  const own = (text: string, remedy?: Remedy): Explained => ({ text, quoted: false, remedy })
  const theirs = (remedy?: Remedy): Explained => ({ text: e.message, quoted: true, remedy })
  switch (e.code) {
    case 'invalid':
    case 'ticket_invalid':
      return theirs()
    case 'not_found':
      return own('This no longer exists; the view shows what there is now.', 'refresh')
    case 'holder_changed':
      return own('Another guest holds this hostname now. Look at the route again before you diagnose it.', 'refresh')
    case 'refused':
      return theirs('look-again')
    case 'unavailable':
      return theirs('try-again')
    case 'forbidden':
      if (e.missing === undefined && e.message === socketRefused) {
        return own("pco's web process may not use the daemon's socket; the troubleshooting guide says why.")
      }
      return theirs()
    case 'unauthenticated':
      return own('Your session has ended. Sign in again; what you did last was not sent again.', 'sign-in')
    case 'proxmox_unreachable':
      return own("Proxmox VE on this node does not answer, or presents another certificate than the node's.", 'try-again')
    case 'daemon_unreachable':
      return own('The pco daemon does not answer. Published routes keep working; nothing changes until it is back.', 'try-again')
    case 'rate_limited':
      return own(e.retryAfter ? `Try again in ${e.retryAfter} s.` : 'Try again in a moment.')
    case 'too_large':
      return own('The request is too large.')
    case 'no_route':
    case 'method_not_allowed':
      return own('The daemon is another version than this page. Reload the page to use the new one.', 'reload')
    case codeTimeout:
      return e.write ? own(timeoutWriteText, 'reload') : own(timeoutReadText, 'try-again')
    case codeNetwork:
      return own("This page cannot reach pco's web process. Check that pco-web.service runs on the node.", 'try-again')
  }
  // internal, unsupported_media_type and what a newer daemon may say
  const time = at.toTimeString().slice(0, 8)
  return own(`Something went wrong in pco at ${time}. journalctl -u pco on the node has the details.`)
}
