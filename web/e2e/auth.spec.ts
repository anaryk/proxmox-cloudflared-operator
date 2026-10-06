import type { Page } from '@playwright/test'

import { admin, csrfOf, expect, populated, reader, test, withTicket } from './fixtures'

// Signing in with the session of Proxmox VE in the browser or with a pasted
// API token, signing out, a session that ends under the page, and what each
// role sees.

const overview = (page: Page) => page.getByRole('heading', { level: 1, name: 'Overview' })
const signInPage = (page: Page) => page.getByRole('heading', { level: 1, name: 'Sign in to pco' })
const stillInProxmox = 'You are signed out of pco. You are still signed in to Proxmox VE in this browser.'

// whoIs opens the user menu and says who it names, and how.
async function whoIs(page: Page): Promise<string> {
  await page.getByRole('button', { name: /user menu$/ }).click()
  const who = page.locator('.menu-who')
  await expect(who).toBeVisible()
  const text = (await who.innerText()).replace(/\s+/g, ' ').trim()
  await page.keyboard.press('Escape')
  return text
}

// The actions only an admin may take, as their buttons read. A reader sees
// none of them, or sees them disabled with what they need.
const adminAction = /^(Approve|Revoke|Resolve|Confirm|Apply|Adopt|Sync|Add|Save|Import|Delete|Remove|Restart|Rotate|Check write access|Start publishing)\b/

test('an admin with a Proxmox VE session lands on the overview', async ({ page, context, pco }) => {
  await withTicket(context, pco, admin)
  await page.goto('/')
  await expect(overview(page)).toBeVisible()
  await expect(page).toHaveURL(`${pco.url}/`)
  expect(await whoIs(page)).toBe('alice@pve admin · signed in with the Proxmox VE session')
})

test('a reader sees no admin action, and of the doctor the counts only', async ({ page, context, pco }) => {
  await withTicket(context, pco, reader)
  await page.goto('/')
  await expect(overview(page)).toBeVisible()
  expect(await whoIs(page)).toBe('bob@pve reader · signed in with the Proxmox VE session')

  for (const path of ['/', '/routes', '/routes/plan', '/guests', '/edge/credentials', '/edge/tunnels', '/settings', '/doctor']) {
    await page.goto(path)
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
    await expect(page.getByRole('button', { name: adminAction, disabled: false }), path).toHaveCount(0)
  }

  const res = await page.request.post('/api/v1/doctor', { data: {}, headers: { Origin: pco.url, 'Pco-Csrf': await csrfOf(page) } })
  expect(res.status()).toBe(200)
  expect(Object.keys((await res.json()) as object).sort()).toEqual(['at', 'fail', 'ok', 'warn'])
})

test('without a Proxmox VE session the sign-in page shows', async ({ page }) => {
  await page.goto('/routes')
  await expect(signInPage(page)).toBeVisible()
  await expect(page).toHaveURL(/\/signin\?next=%2Froutes$/)
  await expect(page.getByText(/in this browser at the same address, in another tab, then come back/)).toBeVisible()
  await expect(page.getByLabel('API token', { exact: true })).toBeVisible()
})

test('a pasted API token signs in, and the page goes where it was asked for', async ({ page }) => {
  await page.goto('/routes')
  await page.getByLabel('API token', { exact: true }).fill(admin.token)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Routes' })).toBeVisible()
  await expect(page).toHaveURL(/\/routes$/)
  expect(await whoIs(page)).toBe('alice@pve!pco admin · signed in with an API token')
})

test('a token Proxmox VE refuses, and one of the wrong form, are said so', async ({ page, watch }) => {
  watch.allow(/^console error on \S+: Failed to load resource: the server responded with a status of 400\b/)
  await page.goto('/')
  const token = page.getByLabel('API token', { exact: true })
  const signIn = page.getByRole('button', { name: 'Sign in', exact: true })

  await token.fill('alice@pve!pco=00000000-0000-4000-8000-000000000000')
  await signIn.click()
  await expect(page.getByText('Proxmox VE did not accept the token')).toBeVisible()

  await token.fill('alice@pve:not-a-token')
  await signIn.click()
  await expect(page.getByText('This is not an API token of the form user@realm!tokenid=secret.')).toBeVisible()
  await expect(signInPage(page)).toBeVisible()
})

test('after a sign-out a reload stays signed out until the user asks', async ({ page, context, pco }) => {
  await withTicket(context, pco, admin)
  await page.goto('/')
  await expect(overview(page)).toBeVisible()

  await page.getByRole('button', { name: /user menu$/ }).click()
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page.getByText(stillInProxmox)).toBeVisible()

  await page.reload()
  await expect(page.getByText(stillInProxmox)).toBeVisible()
  await expect(overview(page)).toHaveCount(0)

  await page.getByRole('button', { name: 'Sign in with the Proxmox VE session' }).click()
  await expect(overview(page)).toBeVisible()
})

test('a Proxmox VE session that ends opens the sign-in over the page', async ({ page, context, pco, fake }) => {
  await withTicket(context, pco, admin)
  await page.goto('/')
  await expect(overview(page)).toBeVisible()

  // The user signs out of Proxmox VE, which removes the cookie; the page
  // learns at its next call, here the read of a new state.
  await context.clearCookies({ name: 'PVEAuthCookie' })
  const st = populated()
  st.problems = [...(st.problems ?? []), 'a problem of the new state']
  await fake.post('/state', st)

  const dialog = page.getByRole('dialog', { name: 'Your session has ended' })
  await expect(dialog).toBeVisible()
  await expect(dialog.getByLabel('API token', { exact: true })).toBeVisible()
  await expect(overview(page)).toBeAttached()
})
