import type { Page } from '@playwright/test'

import { adminAction, expect, reader, rowsOf, signIn, test } from './fixtures'

// The reader scenario: bob@pve has Sys.Audit and may audit qemu/101 and
// lxc/205. pco web filters what the daemon says to what he may see, and
// the pages offer him no admin action.

test.use({ scenario: 'reader' })

const tableRows = (page: Page, name: string) => rowsOf(page.getByRole('table', { name }))

test('the routes are those of the guests the reader may audit, and the manual ones', async ({ page, pco }) => {
  await signIn(page, pco, '/routes', reader)
  await expect(page.getByRole('main').getByRole('status').filter({ hasText: /routes$/ })).toHaveText('5 routes')
  await expect(tableRows(page, 'Routes')).toHaveText([
    /^active ?app\.example\.com.*qemu\/101/,
    /^active ?grafana\.example\.com.*lxc\/205/,
    /^held ?old\.example\.com.*qemu\/101/,
    /^active ?status\.example\.com.*manual\/status/,
    /^active ?www\.example\.com.*qemu\/101/,
  ])
})

test('the guests are those the reader may audit, and none of them waits', async ({ page, pco }) => {
  await signIn(page, pco, '/guests', reader)
  await expect(tableRows(page, 'All tagged guests')).toHaveText([/^qemu\/101 \(web-1\)/, /^lxc\/205 \(grafana\)/])
  await expect(tableRows(page, 'Guests waiting for approval')).toHaveText(['No guest waits for approval.'])
  await expect(page.getByRole('link', { name: /guests? waits? for approval/ })).toHaveCount(0)
})

test('the map draws the chains of those guests only', async ({ page, pco, viewport }) => {
  test.skip((viewport?.width ?? 0) < 720, 'below 720 px the Overview shows the chain list instead of the map')
  await signIn(page, pco, '/', reader)
  const map = page.getByRole('group', { name: 'Flow map' })
  await expect(map.getByRole('button', { name: /^Guest web-1 qemu\/101/ })).toBeVisible()
  await expect(map.getByRole('button', { name: /^Guest grafana lxc\/205/ })).toBeVisible()
  await expect(map.getByRole('button', { name: /^Guest / })).toHaveCount(2)
  await expect(map.getByRole('button', { name: /^Address 10\.0\.5\.20 of manual\/status/ })).toBeVisible()
  await expect(map.getByRole('button', { name: /qemu\/105|lxc\/210|qemu\/102/ })).toHaveCount(0)
})

test('no page offers the reader an admin action', async ({ page, pco }) => {
  await signIn(page, pco, '/', reader)
  for (const path of [
    '/',
    '/routes',
    '/routes/www.example.com?owner=qemu%2F101',
    '/routes/plan',
    '/routes/manual/status',
    '/guests',
    '/guests/qemu/101',
    '/guests/claims',
    '/networks',
    '/edge/credentials',
    '/edge/credentials/cred1',
    '/edge/zones',
    '/edge/zones/example.com',
    '/edge/tunnels',
    '/edge/tunnels/acc1',
    '/events',
    '/doctor',
    '/settings',
    '/setup',
  ]) {
    await page.goto(path)
    await expect(page.getByRole('heading', { level: 1 }), path).toBeVisible()
    await expect(page.getByRole('main').locator('.skeleton'), path).toHaveCount(0)
    await expect(page.getByRole('button', { name: adminAction, disabled: false }), path).toHaveCount(0)
  }
  // Disabled, an action says what it needs.
  await page.goto('/guests')
  await expect(page.getByText('needs Sys.Modify on /').first()).toBeVisible()
})

test('the doctor gives the reader its counts only', async ({ page, pco }) => {
  await signIn(page, pco, '/doctor', reader)
  await page.getByRole('button', { name: 'Run the checks' }).click()
  const result = page.getByRole('region', { name: 'Result' })
  await expect(result.getByRole('status')).toContainText(/^\d+ ok, \d+ warnings?, \d+ failures?, run at/)
  await expect(result).toContainText('The findings are for admins: they name guests waiting for approval and hostnames of conflicts and lost markers that a reader may not see.')
  await expect(page.getByRole('region', { name: 'Failures' })).toHaveCount(0)
})
