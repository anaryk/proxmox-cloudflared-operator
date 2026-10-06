import type { Page } from '@playwright/test'

import { admin, expect, test, withTicket } from './fixtures'

// The first-run setup against a daemon that has no credential yet: it comes
// first, it refuses a token that cannot be used with the list of what to
// grant, and it can be set aside in this browser.

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

async function fill(page: Page, label: string): Promise<void> {
  await page.getByLabel('Label').fill(label)
  await tokenField(page).fill(token)
  await page.getByRole('button', { name: 'Check and add' }).click()
}

test('the setup comes first, refuses a token that cannot be used, and takes one that can', async ({ page, context, pco, fake, watch }) => {
  // the browser reports the answer of 400 to the refused token
  watch.allow(/^console error on \S+\/setup: Failed to load resource: the server responded with a status of 400\b/)
  await withTicket(context, pco, admin)
  await page.goto('/')
  await expect(heading(page, 'First-run setup')).toBeVisible()
  await expect(page).toHaveURL(`${pco.url}/setup`)

  const open = page.getByRole('region', { name: /^API token/ })
  await expect(open.getByRole('listitem')).toHaveText([
    /Account > Cloudflare Tunnel > Edit/,
    /Zone > DNS > Edit/,
    /Zone > Zone > Read/,
  ])
  const links = open.locator('a.btn[target="_blank"]')
  await expect(links).toHaveCount(2)
  for (const a of await links.all()) {
    expect(await a.getAttribute('href')).toMatch(/^https:\/\/dash\.cloudflare\.com\/.*[?&]permissionGroupKeys=%5B.*&name=pco%20on%20\w/)
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
  await expect(page.getByRole('button', { name: /^API token/ })).toHaveAttribute('aria-expanded', 'true')
  await expect(open.getByRole('button', { name: 'Continue to the zones' })).toBeVisible()
  await open.getByRole('button', { name: 'Continue to the zones' }).click()
  await expect(page.getByRole('region', { name: /^Zones/ })).toBeVisible()
})

test('the setup can be skipped in this browser, and says so when it is asked for', async ({ page, context, pco }) => {
  await withTicket(context, pco, admin)
  await page.goto('/')
  await page.getByRole('button', { name: 'Skip for now' }).click()
  await expect(heading(page, 'Overview')).toBeVisible()
  await page.reload()
  await expect(heading(page, 'Overview')).toBeVisible()

  await page.getByRole('link', { name: 'Open the setup' }).click()
  await expect(heading(page, 'First-run setup')).toBeVisible()
  await expect(page.getByText(/You skipped the setup in this browser/)).toBeVisible()
  await page.getByRole('button', { name: 'Show the setup first again' }).click()
  await page.goto('/')
  await expect(page).toHaveURL(`${pco.url}/setup`)
})
