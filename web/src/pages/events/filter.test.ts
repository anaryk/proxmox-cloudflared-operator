import { describe, expect, test } from 'vitest'

import { exportName } from './download'
import { eventsQuery, filterOf, fromLocalInput, isActive, localInput, logKey, searchOf, splitList } from './filter'

describe('the filter in the address', () => {
  test('every filter goes out and comes back', () => {
    const f = {
      level: ['error', 'warn'],
      kind: ['route'],
      route: ['www.example.com'],
      guest: ['qemu/101'],
      account: ['0123456789abcdef0123456789abcdef'],
      text: 'a b&c=d',
      since: '2026-10-01T12:00:00Z',
      until: '2026-10-01T12:05:59.999Z',
    }
    const search = searchOf(f)
    expect(search).toBe(
      'level=error&level=warn&kind=route&route=www.example.com&guest=qemu%2F101&account=0123456789abcdef0123456789abcdef&text=a+b%26c%3Dd&since=2026-10-01T12%3A00%3A00Z&until=2026-10-01T12%3A05%3A59.999Z',
    )
    expect(filterOf(`?${search}`)).toEqual(f)
  })

  test('nothing is no filter', () => {
    expect(searchOf({})).toBe('')
    expect(isActive(filterOf(''))).toBe(false)
    expect(isActive(filterOf('?level=error'))).toBe(true)
  })

  test('an empty parameter sets nothing', () => {
    expect(filterOf('?level=&text=&since=&route=a.example.com&route=')).toEqual({ route: ['a.example.com'] })
  })

  test('a time in the address that is no time sets no range, and shows as none', () => {
    expect(filterOf('?since=soon&until=2026-10-01')).toEqual({ until: '2026-10-01T00:00:00Z' })
    expect(isActive(filterOf('?since=soon'))).toBe(false)
  })

  test('a list takes 100 values of the address at most', () => {
    const many = Array.from({ length: 5000 }, (_, i) => `route=r${i}.example.com`).join('&')
    expect(filterOf(`?${many}`).route).toHaveLength(100)
  })

  test('the text keeps its spaces', () => {
    expect(filterOf(`?${searchOf({ text: 'a ' })}`).text).toBe('a ')
  })
})

describe('the query of the daemon', () => {
  test('asks the log for the newest, with what the daemon can filter by', () => {
    const q = new URLSearchParams(
      eventsQuery({ level: ['warn'], route: ['a.example.com', 'b.example.com'], account: ['acc1'], text: 'x', until: '2026-10-01T12:00:00Z', since: '2026-09-01' }, 2000),
    )
    expect(q.get('history')).toBe('1')
    expect(q.get('limit')).toBe('2000')
    expect(q.getAll('level')).toEqual(['warn'])
    expect(q.getAll('route')).toEqual(['a.example.com', 'b.example.com'])
    expect(q.getAll('account')).toEqual(['acc1'])
    // RFC 3339, which is what the daemon reads; the newest up to the end
    expect(q.get('since')).toBe('2026-09-01T00:00:00Z')
    expect(q.get('until')).toBe('2026-10-01T12:00:00Z')
    expect(q.has('text')).toBe(false)
  })

  test('a time that is no time, or of a year RFC 3339 cannot write, is left out, not sent to be refused', () => {
    for (const at of ['yesterday', '+010000-01-01T00:00:00Z', '-000001-01-01T00:00:00Z']) {
      const q = new URLSearchParams(eventsQuery({ since: at, until: at }, 1000))
      expect(q.has('since'), at).toBe(false)
      expect(q.has('until'), at).toBe(false)
    }
  })

  test('what was read is good for a filter that asks the daemon the same', () => {
    expect(logKey({ level: ['error'], text: 'a' })).toBe(logKey({ level: ['error'], text: 'b' }))
    expect(logKey({ level: ['error'] })).not.toBe(logKey({ level: ['warn'] }))
    expect(logKey({ since: '2026-10-01T12:00:00Z' })).not.toBe(logKey({}))
    expect(logKey({ until: '2026-10-01T12:00:00Z' })).not.toBe(logKey({}))
  })
})

describe('the fields of time', () => {
  test('a time is the minute of the browser zone, and back', () => {
    const at = new Date(2026, 9, 1, 14, 30).toISOString()
    expect(localInput(at)).toBe('2026-10-01T14:30')
    expect(fromLocalInput('2026-10-01T14:30')).toBe(at.replace('.000Z', 'Z'))
  })

  test('the end of a range is the end of its minute', () => {
    const end = fromLocalInput('2026-10-01T14:30', true) ?? ''
    expect(Date.parse(end) - Date.parse(fromLocalInput('2026-10-01T14:30') ?? '')).toBe(59_999)
    expect(localInput(end)).toBe('2026-10-01T14:30')
  })

  test('what is no time is empty', () => {
    expect(localInput('')).toBe('')
    expect(localInput('soon')).toBe('')
    expect(localInput(undefined)).toBe('')
    expect(fromLocalInput('')).toBeUndefined()
    expect(fromLocalInput('not a time')).toBeUndefined()
  })
})

test('a list is set apart by commas and spaces', () => {
  expect(splitList('a, b  c,,d ')).toEqual(['a', 'b', 'c', 'd'])
  expect(splitList('')).toEqual([])
})

describe('the name of an export', () => {
  const now = new Date(2026, 9, 1, 9, 5, 7)

  test('has the node and the time', () => {
    expect(exportName('pve1', now)).toBe('pco-events-pve1-20261001-090507.json')
  })

  test('has no character that is not a letter, a digit, a dot or a dash from the node', () => {
    expect(exportName('pve/1 ../x', now)).toBe('pco-events-pve-1-..-x-20261001-090507.json')
    expect(exportName(undefined, now)).toBe('pco-events-20261001-090507.json')
  })
})
