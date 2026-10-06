import type { Page } from '@playwright/test'

import { admin, expect, type FakeDaemon, populated, test, withTicket } from './fixtures'

// The first-run setup against a daemon that has no credential yet: it comes
// first once a cycle has finished, it refuses a token that cannot be used with
// the list of what to grant, and it can be set aside in this browser.

test.use({ scenario: 'first-run' })

const heading = (page: Page, name: string) => page.getByRole('heading', { level: 1, name })
const tokenField = (page: Page) => page.getByRole('textbox', { name: 'Cloudflare API token' })

const token = 'Ab3_dEf-0123456789ghijKLMN'

// A token that can read the zones but not their DNS, as Cloudflare answers
// for it.
const unusable = {
  label: 'edge',
  kind: 'scoped',
  checked: true,
  id: '',
  report: {
    token: { id: 'token-1', status: 'active' },
    accounts: [{ id: 'acc1', name: 'Main' }],
    zones: [{ id: 'zone1', name: 'example.com', status: 'active', accountId: 'acc1' }],
    checks: [{ capability: 'dns.write', scope: 'example.com', scopeId: 'zone1', ok: false, detail: 'grant Zone > DNS > Edit on example.com' }],
    excluded: [],
    deep: false,
    usable: false,
    leftovers: [],
  },
}

// cycleUntil has the fake daemon finish cycles until the page, which learns of
// them from the stream it has connected by then, is at url.
async function cycleUntil(page: Page, fake: FakeDaemon, url: string): Promise<void> {
  await expect(async () => {
    await fake.post('/cycle')
    await expect(page).toHaveURL(url, { timeout: 1000 })
  }).toPass({ timeout: 15_000 })
}

async function fill(page: Page, label: string): Promise<void> {
  await page.getByLabel('Label').fill(label)
  await tokenField(page).fill(token)
  await page.getByRole('button', { name: 'Check and add' }).click()
}

test('the setup comes first, refuses a token that cannot be used, and takes one that can', async ({ page, context, pco, fake, watch }) => {
  // the browser reports the answer of 400 to the refused token
  watch.allow(/^console error on \S+\/setup: Failed to load resource: the server responded with a status of 400\b/)
  await withTicket(context, pco, admin)
  // before its first cycle the daemon says nothing of what is set up
  await page.goto('/')
  await expect(heading(page, 'Overview')).toBeVisible()
  await cycleUntil(page, fake, `${pco.url}/setup`)
  await expect(heading(page, 'First-run setup')).toBeVisible()

  const open = page.getByRole('region', { name: /^Step 1: API token/ })
  await expect(open.getByRole('listitem')).toHaveText([
    /Account > Cloudflare Tunnel > Edit.*add this one yourself/,
    /Zone > DNS > Edit/,
    /Zone > Zone > Read/,
  ])
  const links = open.locator('a.btn[target="_blank"]')
  await expect(links).toHaveCount(2)
  for (const a of await links.all()) {
    const href = (await a.getAttribute('href')) ?? ''
    expect(href).toMatch(/^https:\/\/dash\.cloudflare\.com\/.*[?&]permissionGroupKeys=%5B.*&name=pco%20on%20\w/)
    expect(JSON.parse(new URL(href).searchParams.get('permissionGroupKeys') ?? '')).toEqual([
      { key: 'dns', type: 'edit' },
      { key: 'zone', type: 'read' },
    ])
    expect(await a.getAttribute('rel')).toBe('noopener noreferrer')
  }

  await fake.post('/refuse', {
    method: 'AddCredential',
    code: 'invalid',
    message: 'the token cannot be used: dns.write on example.com: grant Zone > DNS > Edit on example.com',
    credential: unusable,
  })
  await fill(page, 'edge')
  await expect(page.getByRole('alert')).toContainText('the token cannot be used')
  await expect(open.getByText('grant Zone > DNS > Edit on example.com', { exact: true })).toBeVisible()
  await expect(tokenField(page)).toHaveValue('')
  await expect(page.getByRole('button', { name: 'Continue to the zones' })).toHaveCount(0)

  await fill(page, 'edge')
  await expect(open.getByText('Added credential cred1 (edge).')).toBeVisible()
  // the step stays where the admin works in it, though the state moved on
  await expect(page.getByRole('button', { name: /^Step 1: API token/ })).toHaveAttribute('aria-expanded', 'true')
  await expect(open.getByRole('button', { name: 'Continue to the zones' })).toBeVisible()
  await open.getByRole('button', { name: 'Continue to the zones' }).click()
  await expect(page.getByRole('region', { name: /^Step 2: Zones/ })).toBeVisible()
})

test('the setup can be skipped in this browser, and says so when it is asked for', async ({ page, context, pco, fake }) => {
  await withTicket(context, pco, admin)
  await page.goto('/setup')
  await page.getByRole('button', { name: 'Skip for now' }).click()
  await expect(heading(page, 'Overview')).toBeVisible()
  await page.reload()
  await expect(heading(page, 'Overview')).toBeVisible()

  await page.getByRole('link', { name: 'Open the setup' }).click()
  await expect(heading(page, 'First-run setup')).toBeVisible()
  await expect(page.getByText(/You skipped the setup in this browser/)).toBeVisible()
  // the setup does not point at itself
  await expect(page.getByRole('link', { name: 'Open the setup' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Show the setup first again' }).click()
  await page.goto('/')
  await expect(heading(page, 'Overview')).toBeVisible()
  await cycleUntil(page, fake, `${pco.url}/setup`)
})

test('the route step fits the width of a small phone', async ({ page, context, pco, fake }) => {
  await withTicket(context, pco, admin)
  await fake.post('/state', { ...populated(), routes: [], mode: 'observe', waiting: [] })
  await page.setViewportSize({ width: 320, height: 700 })
  await page.goto('/setup')
  await expect(page.getByRole('button', { name: /^Step 4: First route/ })).toHaveAttribute('aria-expanded', 'true')
  await expect(page.getByRole('region', { name: 'Annotate a guest' })).toBeVisible()
  // the steps, not the top bar of the shell, which is wider than this at 320 px
  expect(await page.evaluate(() => (document.querySelector('.wizard')?.scrollWidth ?? Infinity) - (document.querySelector('.wizard')?.clientWidth ?? 0))).toBeLessThanOrEqual(0)
})
