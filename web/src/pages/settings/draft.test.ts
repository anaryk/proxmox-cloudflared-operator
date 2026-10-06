import { expect, test } from 'vitest'

import type { Settings } from '../../api/types.gen'
import settingsFixture from '../../fixtures/settings.json'
import { addAllowHost, draftIssues, draftOf, mergeDraft, settingsOf } from './draft'

const stored = settingsFixture.settings as unknown as Settings

const full: Settings = {
  ...stored,
  allowHosts: ['*.example.com', 'shop.cz'],
  denyHosts: ['secret.example.com'],
  trustStatic: true,
  trustedCIDRs: ['10.0.20.0/24'],
  manualCIDRs: ['10.0.5.0/24', '10.0.6.0/24'],
  zonePins: { 'example.com': 'a1b2c3d4', 'shop.cz': 'e5f6' },
}

test('the draft holds what the form edits as text', () => {
  const d = draftOf(full)
  expect(d.allowHosts).toBe('*.example.com\nshop.cz')
  expect(d.manualCIDRs).toBe('10.0.5.0/24\n10.0.6.0/24')
  expect(d.zonePins).toBe('example.com a1b2c3d4\nshop.cz e5f6')
  expect(d.maxHostnamesPerGuest).toBe('32')
  expect(d.cloudflareBudget).toBe('1000')
  expect(d.pollInterval).toBe('10s')
  expect(d.trustStatic).toBe(true)
  expect(draftOf(stored).allowHosts).toBe('')
  expect(draftOf(stored).zonePins).toBe('')
})

test('what the form makes of a draft is the settings it began with', () => {
  expect(settingsOf(draftOf(full), full)).toEqual(full)
  expect(settingsOf(draftOf(stored), stored)).toEqual(stored)
})

test('empty lists and a false trustStatic are left out, as the daemon leaves them out', () => {
  const s = settingsOf({ ...draftOf(stored), allowHosts: '\n  \n', trustedCIDRs: '', zonePins: ' ' })
  expect(Object.keys(s).sort()).toEqual(Object.keys(stored).sort())
})

test('a list takes one entry a line, and commas and spaces between entries too', () => {
  const s = settingsOf({ ...draftOf(stored), allowHosts: ' a.example.com ,b.example.com\r\n\r\n c.example.com  d.example.com\n', manualCIDRs: '10.0.5.0/24, 10.0.6.0/24' })
  expect(s.allowHosts).toEqual(['a.example.com', 'b.example.com', 'c.example.com', 'd.example.com'])
  expect(s.manualCIDRs).toEqual(['10.0.5.0/24', '10.0.6.0/24'])
})

test('the patterns are written in their normal form once the settings are made', () => {
  const s = settingsOf({ ...draftOf(stored), allowHosts: '*.Example.COM.\nShop.cz', denyHosts: 'ADMIN.example.com', zonePins: 'Example.COM cred-1' })
  expect(s.allowHosts).toEqual(['*.example.com', 'shop.cz'])
  expect(s.denyHosts).toEqual(['admin.example.com'])
  expect(s.zonePins).toEqual({ 'example.com': 'cred-1' })
})

test('a pattern that is not valid stays as typed, for the validation to name', () => {
  expect(settingsOf({ ...draftOf(stored), allowHosts: 'not-a-host\nok.example.com' }).allowHosts).toEqual(['not-a-host', 'ok.example.com'])
})

test('a zone pin is a zone and a credential id, separated by space, a tab or an equals sign', () => {
  const s = settingsOf({ ...draftOf(stored), zonePins: 'a.example\tid-a\nb.example = id-b\nc.example=id-c\nd.example' })
  expect(s.zonePins).toEqual({ 'a.example': 'id-a', 'b.example': 'id-b', 'c.example': 'id-c', 'd.example': '' })
})

test('numbers are whole numbers or not numbers', () => {
  const n = (text: string) => settingsOf({ ...draftOf(stored), cloudflareBudget: text }).cloudflareBudget
  expect(n('500')).toBe(500)
  expect(n(' 500 ')).toBe(500)
  expect(n('-1')).toBe(-1)
  expect(n('')).toBeNaN()
  expect(n('5e2')).toBeNaN()
  expect(n('1.5')).toBeNaN()
  expect(n('ten')).toBeNaN()
})

test('a field the page does not know is kept from the settings it was read in', () => {
  const base = { ...full, somethingNew: 3 } as unknown as Settings
  expect((settingsOf(draftOf(base), base) as unknown as Record<string, unknown>).somethingNew).toBe(3)
})

test('a zone named twice is found in the draft, which the settings could not say', () => {
  expect(draftIssues({ ...draftOf(stored), zonePins: 'a.example id-a\nb.example id-b' })).toEqual([])
  const got = draftIssues({ ...draftOf(stored), zonePins: 'a.example id-a\nb.example id-b\na.example id-c' })
  expect(got).toEqual([{ field: 'zonePins["a.example"]', message: 'zonePins["a.example"]: named on two lines' }])
  expect(draftIssues({ ...draftOf(stored), zonePins: 'a.example id-a\nA.Example. id-b' }).map((i) => i.field)).toEqual(['zonePins["A.Example."]'])
})

test('addAllowHost adds a pattern once and keeps the others', () => {
  const d = draftOf(full)
  expect(addAllowHost(d, 'example.com').allowHosts).toBe('*.example.com\nshop.cz\nexample.com')
  expect(addAllowHost(d, 'SHOP.cz')).toBe(d)
  expect(addAllowHost({ ...d, allowHosts: '' }, '*.example.com').allowHosts).toBe('*.example.com')
  expect(addAllowHost({ ...d, allowHosts: '*.example.com\n\n' }, 'example.com').allowHosts).toBe('*.example.com\nexample.com')
})

test('the edits of a user are put on settings that changed meanwhile, field by field', () => {
  const was = draftOf(stored)
  const mine = { ...was, grace: '2m', denyHosts: 'x.example.com', cloudflareBudget: '500' }
  const now = draftOf({ ...stored, pollInterval: '30s', cloudflareBudget: 700, allowHosts: ['a.example.com'] })
  expect(mergeDraft(mine, was, now)).toEqual({
    ...now,
    grace: '2m',
    denyHosts: 'x.example.com',
    cloudflareBudget: '500',
  })
  expect(mergeDraft(was, was, now)).toEqual(now)
})
