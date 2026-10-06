import type { Page } from '@playwright/test'

import { expect, onPhone, populated, rowsOf, signIn, test } from './fixtures'

// The routes of the populated scenario: the filters of the table, a route's
// detail, its diagnosis, which the daemon runs on the route that holds the
// hostname, and the manual routes, written with the revision they were read
// at.

const status = (page: Page) => page.getByRole('main').getByRole('status').filter({ hasText: /routes$/ })
const total = populated().routes.length
const table = (page: Page) => page.getByRole('table', { name: 'Routes' })
const row = (page: Page, ...texts: string[]) => texts.reduce((rows, text) => rows.filter({ hasText: text }), rowsOf(table(page)))

// A route's detail is beside the table on a desktop, and the page itself on
// a phone.
const detailOf = (page: Page, phone: boolean, hostname: string) => (phone ? page.getByRole('main') : page.getByRole('complementary', { name: hostname }))

test('the state chips, the zone and the search narrow the table', async ({ page, pco }) => {
  await signIn(page, pco, '/routes')
  await expect(status(page)).toHaveText(`${total} routes`)

  const unreachable = page.getByRole('group', { name: 'States' }).getByRole('button', { name: 'unreachable 1' })
  await unreachable.click()
  await expect(unreachable).toHaveAttribute('aria-pressed', 'true')
  await expect(status(page)).toHaveText(`1 of ${total} routes`)
  await expect(rowsOf(table(page))).toHaveCount(1)
  await expect(row(page, 'monitor.example.com')).toContainText('target is not answering')
  await page.getByRole('group', { name: 'States' }).getByRole('button', { name: 'no-zone 2' }).click()
  await expect(status(page)).toHaveText(`3 of ${total} routes`)
  await page.getByRole('button', { name: 'Clear the filter' }).click()
  await expect(status(page)).toHaveText(`${total} routes`)

  await page.getByRole('combobox', { name: 'Zone' }).selectOption('example.info')
  await expect(status(page)).toHaveText(`1 of ${total} routes`)
  await expect(row(page, 'notes.example.info')).toContainText('frozen')
  await page.getByRole('combobox', { name: 'Zone' }).selectOption('')

  // The search reads hostnames, owners, the names of guests and services.
  for (const [text, shown] of [
    ['grafana', 1],
    ['qemu/101', 3],
    ['10.0.0.40', 2],
    ['nothing like it', 0],
  ] as const) {
    await page.getByRole('searchbox', { name: 'Search the routes' }).fill(text)
    await expect(status(page), text).toHaveText(`${shown} of ${total} routes`)
  }
  await expect(table(page)).toContainText('No route matches the filter.')
})

test('a row opens the route, at an address of its own', async ({ page, pco, viewport }) => {
  const phone = onPhone(viewport)
  await signIn(page, pco, '/routes')
  await row(page, 'www.example.com', 'http://10.0.0.11:8080').click()
  await expect(page).toHaveURL(/\/routes\/www\.example\.com\?owner=qemu%2F101$/)
  const detail = detailOf(page, phone, 'www.example.com')
  await expect(detail.getByRole('tab', { name: 'Overview', selected: true })).toBeVisible()
  await expect(detail.getByRole('tablist', { name: 'Route' }).getByRole('tab')).toHaveText(['Overview', 'Diagnosis of the current holder', 'Timeline', 'Claim'])
  const overview = detail.getByRole('tabpanel', { name: 'Overview' })
  await expect(overview).toContainText('http://10.0.0.11:8080')
  await expect(overview).toContainText('vmbr0 · VLAN 20')
  await expect(detail.getByRole('link', { name: 'Open https://www.example.com/' })).toHaveAttribute('rel', 'noopener noreferrer')
  await expect(detail.getByRole('link', { name: 'qemu/102' })).toBeVisible()

  // The address opens the same detail; Escape closes the one beside the
  // table, a link leads back from the page of a phone.
  await page.reload()
  await expect(detailOf(page, phone, 'www.example.com').getByRole('tab', { name: 'Overview' })).toBeVisible()
  if (phone) await page.getByRole('link', { name: 'All routes' }).click()
  else await page.keyboard.press('Escape')
  await expect(page).toHaveURL(/\/routes$/)
  await expect(page.getByRole('tab', { name: 'Overview' })).toHaveCount(0)
})

test('a route reached through the leg of an attached network says how, in its detail and its diagnosis', async ({ page, pco, viewport }) => {
  await signIn(page, pco, '/routes/zabbix.example.com?owner=qemu%2F107')
  const detail = detailOf(page, onPhone(viewport), 'zabbix.example.com')
  const overview = detail.getByRole('tabpanel', { name: 'Overview' })
  await expect(overview).toContainText('http://198.18.1.5:8080')
  await expect(overview).toContainText('pve1 · vmbr2 · tap107i0')
  await expect(overview).toContainText('attach vmbr2: leg up, service 198.18.1.5 → 10.50.0.10')

  await detail.getByRole('tab', { name: 'Diagnosis of the current holder' }).click()
  await detail.getByRole('button', { name: 'Run diagnosis' }).click()
  const steps = detail.getByRole('list', { name: 'Steps of the diagnosis' }).getByRole('listitem')
  await expect(steps.filter({ has: page.locator('.step-name', { hasText: /^path$/ }) })).toHaveCount(1)
})

test('a diagnosis is posted with the hostname, and its steps come at once', async ({ page, pco, fake, viewport }) => {
  await signIn(page, pco, '/routes/monitor.example.com?owner=qemu%2F105')
  const detail = detailOf(page, onPhone(viewport), 'monitor.example.com')
  await detail.getByRole('tab', { name: 'Diagnosis of the current holder' }).click()
  await fake.clearCalls()

  const asked = page.waitForRequest((r) => new URL(r.url()).pathname === '/api/v1/diagnose')
  await detail.getByRole('button', { name: 'Run diagnosis' }).click()
  const request = await asked
  expect(request.method()).toBe('POST')
  expect(request.postDataJSON()).toEqual({ hostname: 'monitor.example.com' })

  const steps = detail.getByRole('list', { name: 'Steps of the diagnosis' }).getByRole('listitem')
  await expect(steps.first()).toBeVisible()
  expect(await steps.count()).toBeGreaterThan(2)
  await expect(detail.getByRole('status').filter({ hasText: 'Run in this browser at' })).toBeVisible()
  await expect(steps.filter({ hasText: 'fail' }).first()).toBeVisible()
  expect((await fake.calls('Diagnose')).map((c) => c.args)).toEqual([{ hostname: 'monitor.example.com' }])
})

test('a route that lost its hostname has no diagnosis, and leads to the holder', async ({ page, pco, viewport }) => {
  const phone = onPhone(viewport)
  await signIn(page, pco, '/routes/www.example.com?owner=qemu%2F102')
  const detail = detailOf(page, phone, 'www.example.com')
  await expect(detail.getByRole('tablist', { name: 'Route' }).getByRole('tab')).toHaveText(['Overview', 'Timeline', 'Claim'])
  await expect(detail).toContainText('This route lost its hostname to qemu/101.')
  await detail.getByRole('link', { name: 'the route of qemu/101' }).click()
  await expect(page).toHaveURL(/\/routes\/www\.example\.com\?owner=qemu%2F101$/)
  await expect(detailOf(page, phone, 'www.example.com').getByRole('tab', { name: 'Diagnosis of the current holder' })).toBeVisible()
})

test('Sync now asks the daemon for a cycle', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/routes')
  await fake.clearCalls()
  await page.getByRole('button', { name: 'Sync now' }).click()
  await expect(page.getByText('A cycle was requested.')).toBeVisible()
  expect(await fake.calls('Trigger')).toHaveLength(1)
})

test('a rejected route offers its own hostname to allowHosts, and saves nothing', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/routes')
  await fake.clearCalls()
  const link = row(page, '*.store.example.com').getByRole('link', { name: 'Add to allowHosts' })
  await expect(link).toHaveAttribute('href', '/settings?addAllowHost=*.store.example.com&owner=lxc%2F210')
  await link.click()
  await expect(page.getByRole('heading', { level: 1, name: 'Settings' })).toBeVisible()
  await expect(page.getByRole('main')).toContainText('*.store.example.com')
  expect(await fake.calls('SaveSettings')).toEqual([])
})

test('a manual route is made, changed and deleted at the revision it was read at', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/routes/manual/new')
  await fake.clearCalls()
  await page.getByRole('textbox', { name: 'Id' }).fill('docs')
  await page.getByRole('textbox', { name: 'Hostname' }).fill('docs.example.com')
  await page.getByRole('textbox', { name: 'Address' }).fill('10.0.5.21')
  await page.getByRole('textbox', { name: 'Port' }).fill('8080')
  await page.getByRole('button', { name: 'Make the route' }).click()
  await expect.poll(async () => (await fake.calls('CreateManualRoute')).map((c) => c.args)).toEqual([
    { id: 'docs', rev: 0, hostname: 'docs.example.com', target: { kind: 'address', scheme: 'http', addr: '10.0.5.21', port: 8080 }, options: {} },
  ])

  // The page of the new route, read again from the daemon.
  await expect(page).toHaveURL(/\/routes\/manual\/docs$/)
  await expect(page.getByRole('textbox', { name: 'Port' })).toHaveValue('8080')
  await page.getByRole('textbox', { name: 'Port' }).fill('8081')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect.poll(async () => (await fake.calls('UpdateManualRoute')).map((c) => c.args)).toEqual([
    { id: 'docs', rev: 1, route: { id: '', rev: 1, hostname: 'docs.example.com', target: { kind: 'address', scheme: 'http', addr: '10.0.5.21', port: 8081 }, options: {} } },
  ])

  await expect(page.getByRole('textbox', { name: 'Port' })).toHaveValue('8081')
  await page.getByRole('button', { name: 'Delete' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: /^Delete/ }).click()
  await expect.poll(async () => (await fake.calls('DeleteManualRoute')).map((c) => c.args)).toEqual([{ id: 'docs', rev: 2 }])
  await expect(page).toHaveURL(/\/routes$/)
})

test('an address outside manualCIDRs is refused at the field before anything is sent', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/routes/manual/new')
  await fake.clearCalls()
  await page.getByRole('textbox', { name: 'Hostname' }).fill('docs.example.com')
  await page.getByRole('textbox', { name: 'Address' }).fill('192.168.1.10')
  await page.getByRole('textbox', { name: 'Port' }).fill('8080')
  await page.getByRole('button', { name: 'Make the route' }).click()
  await expect(page.getByRole('textbox', { name: 'Address' })).toHaveAttribute('aria-invalid', 'true')
  expect(await fake.calls('CreateManualRoute')).toEqual([])
})
