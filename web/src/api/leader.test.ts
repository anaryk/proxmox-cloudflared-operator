import { expect, test, vi } from 'vitest'

import { type Channel, type Locks, shareStream } from './leader'
import type { Link, Notice, StreamOptions } from './stream'

// The lock manager of one browser: a lock is held until the promise of its
// holder settles, then the next waiting request gets it.
class FakeLocks implements Locks {
  #held = false
  #queue: (() => void)[] = []

  request(_name: string, callback: () => Promise<void>): Promise<unknown> {
    return new Promise((done) => {
      const grant = () => {
        this.#held = true
        void callback().then(() => {
          this.#held = false
          done(undefined)
          this.#queue.shift()?.()
        })
      }
      if (this.#held) this.#queue.push(grant)
      else grant()
    })
  }
}

// The channels of one browser: a message reaches every other channel.
class Bus {
  channels = new Set<FakeChannel>()
}

class FakeChannel implements Channel {
  #listeners: ((e: MessageEvent) => void)[] = []
  constructor(private bus: Bus) {
    bus.channels.add(this)
  }
  postMessage(message: unknown): void {
    for (const c of this.bus.channels) {
      if (c === this) continue
      queueMicrotask(() => c.deliver(structuredClone(message)))
    }
  }
  deliver(data: unknown) {
    for (const l of this.#listeners) l(new MessageEvent('message', { data }))
  }
  addEventListener(_type: 'message', listener: (e: MessageEvent) => void): void {
    this.#listeners.push(listener)
  }
  close(): void {
    this.bus.channels.delete(this)
  }
}

function tab(locks: Locks, bus: Bus) {
  const notices: Notice[] = []
  const links: Link[] = []
  let stream: StreamOptions | undefined
  const opened = vi.fn()
  const resumed = vi.fn()
  const shared = shareStream({
    locks,
    channel: new FakeChannel(bus),
    open: (o) => {
      stream = o
      opened(o.lastEventId?.())
      return { close: () => (stream = undefined), resume: resumed }
    },
    onNotice: (n) => notices.push(n),
    onLink: (l) => links.push(l),
    lastEventId: () => 'b:41',
  })
  return {
    shared,
    notices,
    links,
    opened,
    resumed,
    emit: (n: Notice) => stream?.onNotice(n, ''),
    link: (l: Link) => stream?.onLink(l),
  }
}

const hello: Notice = { kind: 'hello', data: { boot: 'b', version: '1.3.0', seq: 41, digest: 'd', pollInterval: '10s' } }
const settle = () => new Promise((r) => setTimeout(r, 0))

test('the first tab holds the stream and the second listens; when the first goes, the second takes over', async () => {
  const locks = new FakeLocks()
  const bus = new Bus()
  const a = tab(locks, bus)
  const b = tab(locks, bus)
  await settle()
  expect(a.opened).toHaveBeenCalledTimes(1)
  expect(b.opened).not.toHaveBeenCalled()
  expect([a.shared.leading(), b.shared.leading()]).toEqual([true, false])

  a.emit(hello)
  a.link({ state: 'open', since: '2026-10-05T12:00:00Z' })
  await settle()
  expect(a.notices).toEqual([hello])
  expect(b.notices).toEqual([hello])
  expect(b.links).toEqual([{ state: 'open', since: '2026-10-05T12:00:00Z' }])

  a.shared.close()
  await settle()
  // the second opens its own, resuming after the last event it holds
  expect(b.opened).toHaveBeenCalledWith('b:41')
  expect(b.shared.leading()).toBe(true)
  b.emit({ kind: 'state', data: { at: 'x', finishedAt: 'y', digest: 'e' } })
  expect(b.notices.at(-1)?.kind).toBe('state')
})

test('a tab that opens later is told the last hello, upstream and link at once', async () => {
  const locks = new FakeLocks()
  const bus = new Bus()
  const a = tab(locks, bus)
  await settle()
  a.emit(hello)
  a.emit({ kind: 'upstream', data: { up: false, since: '2026-10-05T11:59:00Z' } })
  a.link({ state: 'open', since: '2026-10-05T12:00:00Z' })
  const b = tab(locks, bus)
  await settle()
  await settle()
  expect(b.notices.map((n) => n.kind)).toEqual(['hello', 'upstream'])
  expect(b.links).toEqual([{ state: 'open', since: '2026-10-05T12:00:00Z' }])
})

test('a sign-in in a tab that listens opens the stream of the leader again', async () => {
  const locks = new FakeLocks()
  const bus = new Bus()
  const a = tab(locks, bus)
  const b = tab(locks, bus)
  await settle()
  b.shared.resume()
  await settle()
  expect(a.resumed).toHaveBeenCalledTimes(1)
})

test('without the Web Locks API every tab holds its own stream', () => {
  const opened = vi.fn(() => ({ close: () => {}, resume: () => {} }))
  shareStream({ open: opened, onNotice: () => {}, onLink: () => {}, lastEventId: () => '' })
  expect(opened).toHaveBeenCalledTimes(1)
})
