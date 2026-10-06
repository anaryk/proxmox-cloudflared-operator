import { cleanup, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test } from 'vitest'

import type { Report } from '../../api/types.gen'
import populated from '../../fixtures/populated.json'
import { fakeStore } from '../../test/store'
import { renderPage } from '../testing'
import { Checklist, expiryNote, groupChecks, leftOutText, usableText } from './Checklist'

afterEach(cleanup)

const day = 24 * 3_600_000

function report(patch: Partial<Report> = {}): Report {
  return {
    token: { id: 't1', status: 'active' },
    accounts: [{ id: 'acc1', name: 'Main' }],
    zones: [
      { id: 'zone1', name: 'example.com', status: 'active', accountId: 'acc1' },
      { id: 'zone2', name: 'example.org', status: 'active', accountId: 'acc1' },
    ],
    checks: [
      { capability: 'token', ok: true },
      { capability: 'accounts', ok: true },
      { capability: 'zones', ok: true },
      { capability: 'dns.read', scope: 'example.com', scopeId: 'zone1', ok: true },
      { capability: 'dns.write', scope: 'example.com', scopeId: 'zone1', ok: false, unanswered: true, detail: 'rate limited' },
      { capability: 'tunnel.read', scope: 'Main', scopeId: 'acc1', ok: false, detail: 'grant Account > Cloudflare Tunnel > Read on Main' },
    ],
    excluded: [{ zone: 'example.org', zoneId: 'zone2', reason: 'no DNS read', detail: 'grant Zone > DNS > Edit on example.org' }],
    deep: true,
    usable: false,
    leftovers: ['_pco-probe-x.example.com'],
    ...patch,
  }
}

describe('groupChecks', () => {
  test('the token, then each account with its zones', () => {
    expect(groupChecks(report()).map((g) => [g.title, g.zone, g.checks.map((c) => c.capability)])).toEqual([
      ['The token', false, ['token', 'accounts', 'zones']],
      ['Account Main', false, ['tunnel.read']],
      ['Zone example.com', true, ['dns.read', 'dns.write']],
    ])
  })

  test('a check of a scope the report does not list comes last, under its scope', () => {
    const r = report({ checks: [...report().checks, { capability: 'dns.read', scope: 'example.net', scopeId: 'zone9', ok: false, detail: 'grant it' }] })
    expect(groupChecks(r).at(-1)).toMatchObject({ title: 'example.net', checks: [{ capability: 'dns.read', scopeId: 'zone9' }] })
  })
})

test('the checklist in the words and marks of the command line', async () => {
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <Checklist report={report()} />)
  const zone = screen.getByRole('region', { name: 'Zone example.com' })
  const lines = within(zone).getAllByRole('listitem')
  expect(lines.map((l) => l.textContent)).toEqual(['✓dns.read on example.com passed', '?dns.write on example.com not known, Cloudflare did not answerrate limited'])
  expect(lines[1]?.className).toContain('check-unanswered')
  const account = screen.getByRole('region', { name: 'Account Main' })
  expect(within(account).getByRole('listitem').textContent).toBe('✗tunnel.read on Main failedgrant Account > Cloudflare Tunnel > Read on Main')
  const left = screen.getByRole('region', { name: 'Zones left out' })
  expect(within(left).getByRole('listitem').textContent).toBe('-example.org left out: no DNS readgrant Zone > DNS > Edit on example.org')
  expect(screen.getByText(/Usable:/).parentElement?.textContent).toBe('Usable: no')
  const leftovers = [...document.querySelectorAll('.checklist > p')].map((p) => p.textContent)
  expect(leftovers).toContain('Probe records left by an earlier check, to be removed by hand: _pco-probe-x.example.com')
  // the groups come first, then the zones left out, then the leftovers
  const order = [...document.querySelectorAll('.check-title')].map((h) => h.textContent)
  expect(order).toEqual(['The token', 'Account Main', 'Zone example.com', 'Zones left out'])
})

test('usable: yes, no, or not known when Cloudflare did not answer', () => {
  expect(usableText(report({ usable: true }))).toBe('yes')
  expect(usableText(report())).toBe('no')
  const unanswered = report({ checks: report().checks.filter((c) => c.capability !== 'tunnel.read') })
  expect(usableText(unanswered)).toBe('not known, Cloudflare did not answer')
})

test('the zones left out, as report.LeftOut says them', () => {
  expect(leftOutText(report())).toBe('example.org left out: no DNS read')
  expect(leftOutText(report({ excluded: [] }))).toBe('')
})

test('an expiry under 30 days is warned of', async () => {
  const now = Date.parse('2026-10-01T12:00:00Z')
  expect(expiryNote('2026-10-31T12:00:00Z', now)).toBeUndefined()
  expect(expiryNote('2026-10-31T11:59:59Z', now)).toEqual({ tone: 'warn', text: 'expires in 29 days' })
  expect(expiryNote('2026-10-02T12:00:00Z', now)).toEqual({ tone: 'warn', text: 'expires in 1 day' })
  expect(expiryNote('2026-10-02T11:00:00Z', now)).toEqual({ tone: 'warn', text: 'expires in less than a day' })
  expect(expiryNote('2026-10-01T12:00:00Z', now)).toEqual({ tone: 'fail', text: 'expired' })
  expect(expiryNote(undefined, now)).toBeUndefined()

  const { store } = await fakeStore({ state: populated })
  renderPage(store, <Checklist report={report({ token: { id: 't1', status: 'active', expiresOn: new Date(Date.now() + 10 * day + 60_000).toISOString() } })} />)
  expect(screen.getByText('expires in 10 days').className).toContain('badge-warn')
})
