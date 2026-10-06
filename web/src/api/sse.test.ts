import { describe, expect, test } from 'vitest'

import { readSse, SseParser } from './sse'

describe('SseParser', () => {
  test('the stream of the daemon, as its golden has it', () => {
    const p = new SseParser()
    const got = p.push(
      'event: hello\ndata: {"boot":"9f2c4e1a0b7d3c55"}\n\n' +
        'id: 9f2c4e1a0b7d3c55:813\nevent: event\ndata: {"seq":813}\n\n' +
        ': ping\n\n' +
        'event: reset\ndata: {"reason":"boot changed"}\n\n',
    )
    expect(got).toEqual([
      { event: 'hello', data: '{"boot":"9f2c4e1a0b7d3c55"}', id: '' },
      { event: 'event', data: '{"seq":813}', id: '9f2c4e1a0b7d3c55:813' },
      // the id stays the last one seen
      { event: 'reset', data: '{"reason":"boot changed"}', id: '9f2c4e1a0b7d3c55:813' },
    ])
  })

  test('CRLF, and a CR alone, end lines', () => {
    const p = new SseParser()
    expect(p.push('event: a\r\ndata: 1\r\n\r\nevent: b\rdata: 2\r\r')).toEqual([
      { event: 'a', data: '1', id: '' },
      { event: 'b', data: '2', id: '' },
    ])
  })

  test('a CRLF split between two chunks is one line end', () => {
    const p = new SseParser()
    expect(p.push('data: x\r')).toEqual([])
    // Not an empty line: the end of the one before.
    expect(p.push('\ndata: y\r')).toEqual([])
    expect(p.push('\n\r')).toEqual([{ event: 'message', data: 'x\ny', id: '' }])
    expect(p.push('\n')).toEqual([])
  })

  test('several data lines join with a line feed; the space after the colon is optional', () => {
    const p = new SseParser()
    expect(p.push('data: first\ndata:second\ndata\ndata:  two spaces\n\n')).toEqual([
      { event: 'message', data: 'first\nsecond\n\n two spaces', id: '' },
    ])
  })

  test('comments, unknown fields, and a message without data are skipped', () => {
    const p = new SseParser()
    expect(p.push(': ping\nretry: 10\nevent: lonely\n\nfoo: bar\ndata: kept\n\n')).toEqual([{ event: 'message', data: 'kept', id: '' }])
  })

  test('messages and lines arrive in pieces', () => {
    const p = new SseParser()
    const text = 'id: b:1\nevent: event\ndata: {"seq":1}\n\nid: b:2\nevent: event\ndata: {"seq":2}\n\n'
    const got = [...text].flatMap((c) => p.push(c))
    expect(got.map((m) => m.id)).toEqual(['b:1', 'b:2'])
    expect(got.map((m) => m.data)).toEqual(['{"seq":1}', '{"seq":2}'])
  })

  test('a leading byte order mark is dropped, an id with NUL ignored, an empty id clears it', () => {
    const p = new SseParser('b:0')
    expect(p.push('﻿id: a\0b\ndata: 1\n\nid\ndata: 2\n\n')).toEqual([
      { event: 'message', data: '1', id: 'b:0' },
      { event: 'message', data: '2', id: '' },
    ])
  })
})

test('readSse decodes the bytes of a body, UTF-8 split across chunks included', async () => {
  const bytes = new TextEncoder().encode('data: Žluťoučký\n\nevent: x\ndata: unfinished\n')
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      c.enqueue(bytes.slice(0, 8))
      c.enqueue(bytes.slice(8))
      c.close()
    },
  })
  const got = []
  for await (const m of readSse(body)) got.push(m)
  expect(got).toEqual([{ event: 'message', data: 'Žluťoučký', id: '' }])
})
