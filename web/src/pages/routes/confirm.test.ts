import { describe, expect, test } from 'vitest'

import type { Waiting } from '../../api/types.gen'
import populated from '../../fixtures/populated.json'
import { canSend, type ConfirmState, difference, effect, needsWord, reduce, request, start } from './confirm'

const waiting = populated.waiting as Waiting[]
const offer = populated.offer
const [removals, zone, tunnel, vanished] = waiting as [Waiting, Waiting, Waiting, Waiting]

const zoneOnly = [zone]
const moreRemovals: Waiting[] = [{ ...removals, items: [...removals.items, 'c.example.com'] }, zone, tunnel, vanished]

function sending(s: ConfirmState): Extract<ConfirmState, { name: 'sending' }> {
  if (s.name !== 'sending') throw new Error(`not sending but ${s.name}`)
  return s
}

describe('the confirmation', () => {
  test('opens on the list and the offer of one state', () => {
    expect(start(offer, waiting)).toEqual({ name: 'reviewing', offer, items: waiting })
  })

  test('removals and vanished guests need the word; a zone alone does not', () => {
    expect(needsWord(waiting)).toBe(true)
    expect(needsWord([vanished])).toBe(true)
    expect(needsWord(zoneOnly)).toBe(false)
    expect(canSend(start(offer, zoneOnly))).toBe(true)
    expect(canSend(start(offer, waiting))).toBe(false)
  })

  test('typing the word lets it send; anything else does not', () => {
    let s = reduce(start(offer, waiting), { type: 'type', text: 'confir' })
    expect(s).toMatchObject({ name: 'typing', typed: 'confir' })
    expect(canSend(s)).toBe(false)
    expect(reduce(s, { type: 'send' })).toBe(s)
    s = reduce(s, { type: 'type', text: 'Confirm' })
    expect(canSend(s)).toBe(false)
    s = reduce(s, { type: 'type', text: 'confirm' })
    expect(canSend(s)).toBe(true)
  })

  test('the offer sent is the one of the list reviewed', () => {
    const s = reduce(reduce(start(offer, waiting), { type: 'type', text: 'confirm' }), { type: 'send' })
    expect(request(sending(s))).toEqual({ confirmDeletes: true, offer })
  })

  test('nothing is sent without a list or an offer', () => {
    expect(canSend(start(offer, []))).toBe(false)
    expect(canSend(start(undefined, zoneOnly))).toBe(false)
    expect(reduce(start(undefined, zoneOnly), { type: 'send' }).name).toBe('reviewing')
  })

  test('a state with the same offer changes nothing', () => {
    const s = start(offer, waiting)
    expect(reduce(s, { type: 'state', offer, waiting })).toBe(s)
    const typed = reduce(s, { type: 'type', text: 'conf' })
    expect(reduce(typed, { type: 'state', offer, waiting })).toBe(typed)
  })

  test('a new offer while reading: changed, with what was added and removed, and it never sends', () => {
    for (const s of [start(offer, waiting), reduce(start(offer, waiting), { type: 'type', text: 'confirm' })]) {
      const changed = reduce(s, { type: 'state', offer: 'aaaaaaaaaaaaaaaa', waiting: moreRemovals.filter((w) => w.kind !== 'unseen-tunnel') })
      expect(changed).toMatchObject({ name: 'changed', oldOffer: offer, newOffer: 'aaaaaaaaaaaaaaaa' })
      if (changed.name !== 'changed') throw new Error('not changed')
      expect(changed.added.map((l) => l.text)).toEqual(['c.example.com'])
      expect(changed.removed.map((l) => l.text)).toEqual([tunnel.detail])
      expect(canSend(changed)).toBe(false)
      expect(reduce(changed, { type: 'send' })).toBe(changed)
      expect(reduce(changed, { type: 'type', text: 'confirm' })).toBe(changed)
    }
  })

  test('changed stays changed, against the list reviewed, even when the old offer comes back', () => {
    let s = reduce(start(offer, waiting), { type: 'state', offer: 'aaaaaaaaaaaaaaaa', waiting: zoneOnly })
    s = reduce(s, { type: 'state', offer: 'bbbbbbbbbbbbbbbb', waiting: moreRemovals })
    expect(s).toMatchObject({ name: 'changed', oldOffer: offer, newOffer: 'bbbbbbbbbbbbbbbb' })
    if (s.name !== 'changed') throw new Error('not changed')
    expect(s.added.map((l) => l.text)).toEqual(['c.example.com'])
    expect(s.removed).toEqual([])
    s = reduce(s, { type: 'state', offer, waiting })
    expect(s).toMatchObject({ name: 'changed', newOffer: offer, added: [], removed: [] })
    expect(canSend(s)).toBe(false)
  })

  test('review the new list: the list and offer of the state now, the word typed again', () => {
    const changed = reduce(reduce(start(offer, waiting), { type: 'type', text: 'confirm' }), { type: 'state', offer: 'bbbbbbbbbbbbbbbb', waiting: moreRemovals })
    const s = reduce(changed, { type: 'review', offer: 'bbbbbbbbbbbbbbbb', waiting: moreRemovals })
    expect(s).toEqual({ name: 'reviewing', offer: 'bbbbbbbbbbbbbbbb', items: moreRemovals })
    expect(canSend(s)).toBe(false)
    const sent = reduce(reduce(s, { type: 'type', text: 'confirm' }), { type: 'send' })
    expect(request(sending(sent))).toEqual({ confirmDeletes: true, offer: 'bbbbbbbbbbbbbbbb' })
  })

  test('a state while sending leaves the answer to the daemon', () => {
    const s = reduce(start(offer, zoneOnly), { type: 'send' })
    expect(reduce(s, { type: 'state', offer: 'cccccccccccccccc', waiting: [] })).toBe(s)
  })

  test('the answer: done, with what was accepted', () => {
    const s = reduce(start(offer, zoneOnly), { type: 'send' })
    const done = reduce(s, { type: 'answer', result: { accepted: zoneOnly, leftObserveOnly: true } })
    expect(done).toEqual({ name: 'done', accepted: zoneOnly, leftObserveOnly: true })
    expect(reduce(done, { type: 'state', offer: 'dddddddddddddddd', waiting: [] })).toBe(done)
    expect(reduce(done, { type: 'send' })).toBe(done)
  })

  test('refused by the daemon: its sentence, then the new list', () => {
    const s = reduce(start(offer, zoneOnly), { type: 'send' })
    const refused = reduce(s, { type: 'refused', message: 'what waits for a confirmation changed since it was shown; look again and repeat' })
    expect(refused).toMatchObject({ name: 'refused', message: 'what waits for a confirmation changed since it was shown; look again and repeat' })
    expect(canSend(refused)).toBe(false)
    expect(reduce(refused, { type: 'state', offer: 'eeeeeeeeeeeeeeee', waiting })).toBe(refused)
    expect(reduce(refused, { type: 'review', offer: 'eeeeeeeeeeeeeeee', waiting })).toEqual(start('eeeeeeeeeeeeeeee', waiting))
  })

  test('another failure keeps the list on screen, with the word typed, and says what went wrong', () => {
    const typed = reduce(reduce(start(offer, waiting), { type: 'type', text: 'confirm' }), { type: 'send' })
    expect(reduce(typed, { type: 'failed', message: 'a cycle is running' })).toEqual({
      name: 'typing',
      offer,
      items: waiting,
      typed: 'confirm',
      error: 'a cycle is running',
    })
    const plain = reduce(start(offer, zoneOnly), { type: 'send' })
    expect(reduce(plain, { type: 'failed', message: 'a cycle is running' })).toEqual({ name: 'reviewing', offer, items: zoneOnly, error: 'a cycle is running' })
  })

  test('answers that come at another time change nothing', () => {
    const s = start(offer, zoneOnly)
    expect(reduce(s, { type: 'answer', result: { accepted: [], leftObserveOnly: false } })).toBe(s)
    expect(reduce(s, { type: 'refused', message: 'x' })).toBe(s)
    expect(reduce(s, { type: 'failed', message: 'x' })).toBe(s)
    expect(reduce(s, { type: 'review', offer: 'x', waiting: [] })).toBe(s)
  })
})

test('difference by line: the sentence of an entry and each of its items', () => {
  expect(difference(waiting, waiting)).toEqual({ added: [], removed: [] })
  const d = difference([removals], [{ ...removals, items: ['b.example.com', 'd.example.com'] }])
  expect(d.added).toEqual([{ kind: 'dns-removals', subject: '', text: 'd.example.com' }])
  expect(d.removed).toEqual([{ kind: 'dns-removals', subject: '', text: 'a.example.com' }])
})

test('the button names the effect', () => {
  expect(effect(waiting)).toBe('Accept 2 removals, 2 vanished guests, 1 zone that is gone and 1 tunnel that is gone at the next run')
  expect(effect([{ ...removals, items: Array.from({ length: 6 }, (_, i) => `r${i}.example.com`) }, vanished])).toBe(
    'Accept 6 removals and 2 vanished guests at the next run',
  )
  expect(effect([zone])).toBe('Accept 1 zone that is gone at the next run')
  expect(effect([])).toBe('Nothing to accept')
})
