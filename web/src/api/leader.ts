// One stream per browser (spec-ui 7.2): the tabs elect one with the Web Locks
// API, and that tab passes every notice to the others over a
// BroadcastChannel. When it closes, the lock goes to the next tab, which
// opens the stream. pco web allows 3 streams per session, and a user with
// ten tabs open is one session.

import type { Link, Notice, StreamHandle, StreamOptions } from './stream'

export const lockName = 'pco.stream'
export const channelName = 'pco'

// The parts of LockManager and BroadcastChannel used here.
export interface Locks {
  request(name: string, callback: () => Promise<void>): Promise<unknown>
}

export interface Channel {
  postMessage(message: unknown): void
  addEventListener(type: 'message', listener: (e: MessageEvent) => void): void
  close(): void
}

type Message =
  | { t: 'notice'; n: Notice; id: string }
  | { t: 'link'; link: Link }
  // A tab that just opened asks for what it missed; the leader answers.
  | { t: 'ask' }
  // A tab signed in again: the leader opens its stream again.
  | { t: 'resume' }

export interface ShareOptions {
  locks?: Locks
  channel?: Channel
  open: (o: StreamOptions) => StreamHandle
  onNotice: (n: Notice, id: string) => void
  onLink: (l: Link) => void
  lastEventId: () => string
}

export interface Shared {
  close: () => void
  resume: () => void
  leading: () => boolean
}

const isMessage = (m: unknown): m is Message => typeof m === 'object' && m !== null && typeof (m as { t?: unknown }).t === 'string'

export function shareStream(o: ShareOptions): Shared {
  let stream: StreamHandle | undefined
  let release: (() => void) | undefined
  let closed = false
  // What a tab that opens later needs to know at once: the last of these.
  const kept = new Map<string, { n: Notice; id: string }>()
  let link: Link | undefined

  const post = (m: Message) => o.channel?.postMessage(m)

  const lead = () => {
    stream = o.open({
      lastEventId: o.lastEventId,
      onNotice: (n, id) => {
        if (n.kind === 'hello' || n.kind === 'upstream' || n.kind === 'state' || n.kind === 'traffic') kept.set(n.kind, { n, id })
        if (n.kind === 'reset') kept.delete('state')
        o.onNotice(n, id)
        post({ t: 'notice', n, id })
      },
      onLink: (l) => {
        link = l
        o.onLink(l)
        post({ t: 'link', link: l })
      },
    })
  }

  o.channel?.addEventListener('message', (e) => {
    const m: unknown = e.data
    if (!isMessage(m) || closed) return
    switch (m.t) {
      case 'notice':
        if (!stream) o.onNotice(m.n, m.id)
        break
      case 'link':
        if (!stream) o.onLink(m.link)
        break
      case 'ask':
        if (!stream) break
        for (const kind of ['hello', 'upstream', 'state', 'traffic']) {
          const k = kept.get(kind)
          if (k) post({ t: 'notice', n: k.n, id: k.id })
        }
        if (link) post({ t: 'link', link })
        break
      case 'resume':
        stream?.resume()
        break
    }
  })

  if (!o.locks || !o.channel) {
    // Alone: no other tab can be told, so this one holds its own stream.
    lead()
  } else {
    void o.locks
      .request(lockName, () => {
        if (closed) return Promise.resolve()
        lead()
        return new Promise<void>((resolve) => {
          release = resolve
        })
      })
      .catch(() => {
        // The lock manager refused: lead alone rather than not at all.
        if (!closed && !stream) lead()
      })
    post({ t: 'ask' })
  }

  return {
    close: () => {
      closed = true
      stream?.close()
      stream = undefined
      release?.()
      o.channel?.close()
    },
    resume: () => {
      if (stream) stream.resume()
      else post({ t: 'resume' })
    },
    leading: () => stream !== undefined,
  }
}
