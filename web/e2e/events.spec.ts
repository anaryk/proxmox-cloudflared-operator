import { readFileSync } from 'node:fs'

import type { Page } from '@playwright/test'

import { expect, onPhone, reader, rowsOf, signIn, test } from './fixtures'

// The events, live and from the log on the node, filtered by what the
// address says, and the doctor, which runs only when asked: an admin gets
// the findings, a reader the counts.

const events = (page: Page) => page.getByRole('table', { name: 'Events' })
const rows = (page: Page) => rowsOf(events(page))

test('the filters are in the address, and the page opens on them again', async ({ page, pco }) => {
  await signIn(page, pco, '/events')
  await expect(rows(page).first()).toBeVisible()
  const filters = page.getByRole('search', { name: 'Filter the events' })
  await filters.getByRole('checkbox', { name: 'warn' }).check()
  await expect(page).toHaveURL(/[?&]level=warn/)
  for (const r of await rows(page).all()) await expect(r).toContainText('warn')

  await filters.getByRole('textbox', { name: 'Route' }).fill('monitor.example.com')
  await expect(page).toHaveURL(/[?&]route=monitor\.example\.com/)
  await expect(rows(page)).toHaveCount(1)
  await expect(rows(page).first()).toContainText('qemu/105: unreachable (target is not answering)')

  await page.reload()
  await expect(page.getByRole('search', { name: 'Filter the events' }).getByRole('checkbox', { name: 'warn' })).toBeChecked()
  await expect(page.getByRole('search', { name: 'Filter the events' }).getByRole('textbox', { name: 'Route' })).toHaveValue('monitor.example.com')
  await expect(rows(page)).toHaveCount(1)
})

test('new events come live, and not while the table is paused', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/events')
  await expect(rows(page).first()).toBeVisible()
  await fake.post('/event', { level: 'info', kind: 'egress', subject: 'egress', message: 'the first event of the test' })
  await expect(rows(page).first()).toContainText('the first event of the test')

  await page.getByRole('switch', { name: 'Live' }).click()
  await expect(page.getByRole('switch', { name: 'Live' })).not.toBeChecked()
  await fake.post('/event', { level: 'info', kind: 'egress', subject: 'egress', message: 'the second event of the test' })
  await expect(events(page).getByText('the first event of the test')).toBeVisible()
  await page.waitForTimeout(1000)
  await expect(events(page).getByText('the second event of the test')).toHaveCount(0)

  await page.getByRole('switch', { name: 'Live' }).click()
  await expect(rows(page).first()).toContainText('the second event of the test')
})

test('the events of a large batch come as one row that loads them', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/events')
  await expect(rows(page).first()).toBeVisible()
  const batch = Array.from({ length: 40 }, (_, i) => ({ level: 'info', kind: 'route', subject: `app-${i}.example.com`, message: `qemu/101: active (batch ${i})` }))
  await fake.post('/event', batch)
  const gap = rows(page).filter({ hasText: '8 events of one cycle' })
  await expect(gap).toBeVisible()
  await expect(events(page).getByText('qemu/101: active (batch 31)')).toBeVisible()
  await expect(events(page).getByText('qemu/101: active (batch 39)')).toHaveCount(0)

  await gap.click()
  await expect(events(page).getByText('qemu/101: active (batch 39)')).toBeVisible()
})

test('Load older events reads the log on the node', async ({ page, pco }) => {
  await signIn(page, pco, '/events')
  await expect(rows(page).first()).toBeVisible()
  const read = page.waitForRequest((r) => new URL(r.url()).pathname === '/api/v1/events' && new URL(r.url()).searchParams.get('history') === '1')
  await page.getByRole('button', { name: 'Load older events' }).click()
  const request = await read
  expect(request.method()).toBe('GET')
})

test('the filtered list is exported as JSON', async ({ page, pco }) => {
  await signIn(page, pco, '/events?level=warn')
  await expect(rows(page).first()).toBeVisible()
  const downloading = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Export the list' }).click()
  const download = await downloading
  expect(download.suggestedFilename()).toMatch(/\.json$/)
  const list = JSON.parse(readFileSync((await download.path()) ?? '', 'utf8')) as { level: string }[]
  expect(list.length).toBeGreaterThan(0)
  expect(new Set(list.map((e) => e.level))).toEqual(new Set(['warn']))
})

test('the doctor runs only when asked, and an admin gets its findings', async ({ page, pco, fake, viewport }) => {
  await signIn(page, pco, '/doctor')
  await expect(page.getByRole('region', { name: 'Not run yet' })).toBeVisible()
  expect(await fake.calls('Doctor'), 'the doctor probes the node: the page never runs it by itself').toEqual([])

  const asked = page.waitForRequest((r) => new URL(r.url()).pathname === '/api/v1/doctor')
  await page.getByRole('button', { name: 'Run the checks' }).click()
  expect((await asked).method()).toBe('POST')
  await expect(page.getByRole('main').getByRole('status').filter({ hasText: /\d+ ok, \d+ warnings, \d+ failures, run at/ })).toBeVisible()
  const failures = page.getByRole('region', { name: 'Failures' })
  await expect(failures.getByRole('listitem').first()).toBeVisible()

  // A fix that is a command of pco gets a copy button; any other is text.
  const warnings = page.getByRole('region', { name: 'Warnings' })
  const approve = warnings.getByRole('listitem').filter({ hasText: 'approval lxc/201' })
  await expect(approve.locator('code')).toHaveText('pco guest approve lxc/201')
  await expect(approve.getByRole('button', { name: 'Copy' })).toBeVisible()
  const conflicts = warnings.getByRole('listitem').filter({ hasText: 'record of someone else stands in the way' })
  await expect(conflicts).toContainText('pco adopt <name>')
  await expect(conflicts.getByRole('button', { name: 'Copy' })).toHaveCount(0)
  expect(await fake.calls('Doctor')).toHaveLength(1)

  // The navigation counts the failures of the run in this browser.
  if (onPhone(viewport)) await page.getByRole('button', { name: 'Open navigation' }).click()
  await expect(page.getByRole('navigation').getByRole('link', { name: /^Doctor/ })).toContainText(/\d/)
})

test('a reader gets the counts of the doctor, and why no more', async ({ page, pco }) => {
  await signIn(page, pco, '/doctor', reader)
  await page.getByRole('button', { name: 'Run the checks' }).click()
  const result = page.getByRole('region', { name: 'Result' })
  await expect(result.getByRole('status')).toContainText(/^\d+ ok, \d+ warnings, \d+ failures, run at/)
  await expect(result).toContainText('The findings are for admins')
  await expect(page.getByRole('region', { name: 'Failures' })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Warnings' })).toHaveCount(0)
})

test('the networks are the bridges the routes were proven on', async ({ page, pco }) => {
  await signIn(page, pco, '/networks')
  const direct = rowsOf(page.getByRole('table').first())
  await expect(direct.filter({ hasText: 'vmbr0' })).toHaveCount(2)
  await expect(direct.filter({ hasText: 'no bridge proven' })).toHaveCount(1)
  await expect(page.getByRole('main')).toContainText('It shows no subnet: pco does not know the subnets of a network.')
})
