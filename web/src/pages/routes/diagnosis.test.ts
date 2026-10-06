import { expect, test } from 'vitest'

import type { Step } from '../../api/types.gen'
import steps from '../../fixtures/diagnose.json'
import { changedSince, DiagnosisStore, maxKept } from './diagnosis.ts'

const answer = steps as Step[]

test('a result is kept per hostname with the digest it ran against and the time', async () => {
  const store = new DiagnosisStore(() => Date.parse('2026-10-01T12:01:05Z'))
  await store.run('www.example.com', '5e0c1f7a92b4d3e8', async () => answer)
  const kept = store.get().kept.get('www.example.com')
  expect(kept).toEqual({ steps: answer, digest: '5e0c1f7a92b4d3e8', at: '2026-10-01T12:01:05.000Z' })
  expect(store.get().running).toBeUndefined()
})

test('the state changed since: the digest moved, and the result is kept rather than dropped', async () => {
  const store = new DiagnosisStore()
  await store.run('www.example.com', '5e0c1f7a92b4d3e8', async () => answer)
  const kept = store.get().kept.get('www.example.com')
  if (!kept) throw new Error('nothing kept')
  expect(changedSince(kept, '5e0c1f7a92b4d3e8')).toBe(false)
  expect(changedSince(kept, '77a1000000000000')).toBe(true)
  expect(changedSince(kept, undefined)).toBe(false)
})

test('one at a time: a run while another runs is not made', async () => {
  const store = new DiagnosisStore(() => 1000)
  let finish: (s: Step[]) => void = () => {}
  const first = store.run('a.example.com', 'd1', () => new Promise<Step[]>((r) => (finish = r)))
  expect(store.get().running).toEqual({ hostname: 'a.example.com', since: 1000 })
  let called = false
  await store.run('b.example.com', 'd1', async () => {
    called = true
    return answer
  })
  expect(called).toBe(false)
  finish(answer)
  await first
  expect(store.get().running).toBeUndefined()
  expect([...store.get().kept.keys()]).toEqual(['a.example.com'])
})

test('an error is kept for the hostname until it runs again, and the last result stays', async () => {
  const store = new DiagnosisStore()
  await store.run('www.example.com', 'd1', async () => answer)
  const refused = new Error('rate limited')
  await store.run('www.example.com', 'd2', async () => {
    throw refused
  })
  expect(store.get().failed.get('www.example.com')).toBe(refused)
  expect(store.get().kept.get('www.example.com')?.digest).toBe('d1')
  await store.run('www.example.com', 'd3', async () => answer)
  expect(store.get().failed.has('www.example.com')).toBe(false)
})

test('at most maxKept hostnames, the oldest out', async () => {
  const store = new DiagnosisStore()
  for (let i = 0; i <= maxKept; i++) await store.run(`h${i}.example.com`, 'd', async () => answer)
  expect(store.get().kept.size).toBe(maxKept)
  expect(store.get().kept.has('h0.example.com')).toBe(false)
  expect(store.get().kept.has(`h${maxKept}.example.com`)).toBe(true)
})
