import type { Page } from '@playwright/test'

import { expect, rowsOf, signIn, test } from './fixtures'

// The edge: credentials added, checked and removed as pco credential does
// it, the zones of a served zone that froze, and the tunnels, told apart by
// their account, with the command that rotates one, which no button runs.

const token = 'Ab3_dEf-0123456789ghijKLMN'

test('a token is added, and its write access checked after the question of the CLI', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/edge/credentials')
  await fake.clearCalls()
  await page.getByRole('button', { name: 'Add credential' }).click()
  const form = page.getByRole('form', { name: 'Add a Cloudflare API token' })
  await form.getByRole('textbox', { name: 'Label' }).fill('third')
  await form.getByLabel('Cloudflare API token').fill(token)
  await form.getByRole('button', { name: 'Check and add' }).click()

  await expect(page.getByText(/^Added credential cred3 \(third\)\.$/)).toBeVisible()
  await expect(form.getByLabel('Cloudflare API token')).toHaveValue('')
  expect((await fake.calls('AddCredential')).map((c) => c.args)).toEqual([{ label: 'third', tokenLength: token.length }])
  await expect(page.getByRole('table', { name: 'Credentials' }).getByRole('link', { name: 'third' })).toBeVisible()

  await page.getByRole('table', { name: 'Credentials' }).getByRole('link', { name: 'third' }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Credential cred3' })).toBeVisible()
  await page.getByRole('region', { name: 'third' }).getByRole('button', { name: 'Check write access' }).click()
  const ask = page.getByRole('dialog', { name: 'Check write access' })
  await expect(ask).toContainText('A deep check creates and deletes a test DNS record and a test tunnel. Continue?')
  expect(await fake.calls('CheckCredential')).toEqual([])
  await ask.getByRole('button', { name: 'Check write access' }).click()
  await expect.poll(async () => (await fake.calls('CheckCredential')).map((c) => c.args)).toEqual([{ id: 'cred3', deep: true }])
})

test('a check reads without asking; a removal needs the label typed', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/edge/credentials/cred2')
  await fake.clearCalls()
  const card = page.getByRole('region', { name: 'spare' })
  await card.getByRole('button', { name: 'Check', exact: true }).click()
  await expect.poll(async () => (await fake.calls('CheckCredential')).map((c) => c.args)).toEqual([{ id: 'cred2', deep: false }])
  await expect(card.getByText('never checked')).toHaveCount(0)

  await card.getByRole('button', { name: 'Remove' }).click()
  const dialog = page.getByRole('dialog', { name: 'Remove a credential' })
  const remove = dialog.getByRole('button', { name: 'Remove' })
  await expect(remove).toBeDisabled()
  await dialog.getByRole('textbox').fill('spar')
  await expect(remove).toBeDisabled()
  await dialog.getByRole('textbox').fill('spare')
  await remove.click()
  await expect(page).toHaveURL(/\/edge\/credentials$/)
  await expect(page.getByText('Removed credential cred2.')).toBeVisible()
  expect((await fake.calls('RemoveCredential')).map((c) => c.args)).toEqual([{ id: 'cred2' }])
  await expect(page.getByRole('table', { name: 'Credentials' }).getByRole('link', { name: 'spare', exact: true })).toHaveCount(0)
  await expect(page.getByRole('table', { name: 'Credentials' }).getByRole('link', { name: 'main', exact: true })).toBeVisible()
})

test.describe('a served zone that froze', () => {
  test.use({ scenario: 'frozen' })

  test('every state of a zone, why it froze in full, and a pin saved at its revision', async ({ page, pco, fake }) => {
    await signIn(page, pco, '/edge/zones')
    const zones = page.getByRole('table', { name: 'Zones' })
    for (const [zone, state] of [
      ['example.com', 'served'],
      ['example.eu', 'frozen'],
      ['example.info', 'frozen'],
      ['example.net', 'not served'],
      ['example.org', 'left out'],
      ['example.shop', 'frozen'],
    ] as const) {
      await expect(rowsOf(zones).filter({ has: page.getByRole('link', { name: zone, exact: true }) }), zone).toContainText(state)
    }
    const frozen = page.getByRole('region', { name: 'Frozen and stale zones' })
    await expect(frozen).toContainText('credential cred1 can no longer read the DNS of zone example.shop, which it serves: grant it Zone > DNS > Edit there')
    await expect(frozen).toContainText('the token of credential cred2 could not read the DNS of zone example.eu, which it serves')
    await expect(frozen.getByRole('link', { name: 'Confirm in the plan' }).first()).toHaveAttribute('href', /^\/routes\/plan#/)

    const settings = (await (await page.request.get('/api/v1/settings')).json()) as { rev: number }
    await fake.clearCalls()
    await page.goto('/edge/zones/example.shop')
    await expect(page.getByRole('heading', { level: 1, name: 'example.shop' })).toBeVisible()
    await page.getByRole('button', { name: 'Pin …' }).click()
    const dialog = page.getByRole('dialog', { name: 'Pin example.shop' })
    await dialog.getByRole('radio', { name: 'spare (cred2)' }).check()
    await dialog.getByRole('button', { name: 'Save the pin' }).click()
    await expect(dialog).toHaveCount(0)
    const saved = (await fake.calls('SaveSettings')).map((c) => c.args as { rev: number; settings: { zonePins?: Record<string, string> } })
    expect(saved).toHaveLength(1)
    expect(saved[0]?.rev).toBe(settings.rev)
    expect(saved[0]?.settings.zonePins).toEqual({ 'example.shop': 'cred2' })
  })
})

test.describe('connectors pco does not run', () => {
  test.use({ scenario: 'rogue' })

  const account = (page: Page, name: string | RegExp) => page.getByRole('table', { name: 'Tunnels' }).getByRole('link', { name })

  test('tunnels of one name are told apart by account, and a rotation is a command for root', async ({ page, pco, fake }) => {
    await signIn(page, pco, '/edge/tunnels')
    const tunnels = rowsOf(page.getByRole('table', { name: 'Tunnels' }))
    await expect(tunnels.filter({ hasText: 'pco-abc123' })).toHaveCount(4)
    await expect(tunnels.filter({ hasText: 'Main · 00000000' }).getByRole('cell')).toContainText([/Main · 00000000/, 'pco-abc123', 'yes', /version 3 runs on 1 connector/, /active, ready, 4 connections/, '2'])
    await fake.clearCalls()

    await account(page, 'Main · 00000000').click()
    await expect(page).toHaveURL(/\/edge\/tunnels\/6ab3311bfe7c52d93098e661558a14c2$/)
    const rogues = page.getByRole('region', { name: 'Connectors pco does not run' })
    await expect(rogues.getByRole('heading', { name: '2 connectors pco does not run' })).toBeVisible()
    await expect(rogues.getByRole('listitem')).toHaveText([
      /^0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807 from 198\.51\.100\.7 \(cloudflared 2026\.8\.0\), first seen/,
      /^4f8a2c19-7e3d-4b6a-9c05-1d2e3f405162 from 203\.0\.113\.24 \(cloudflared 2025\.11\.1\), first seen/,
    ])
    await expect(rogues.locator('code')).toHaveText('pco tunnel rotate --account 6ab3311bfe7c52d93098e661558a14c2')
    await expect(rogues).toContainText('Run it as root on the node.')
    await expect(rogues.getByRole('button', { name: /rotate/i })).toHaveCount(0)

    await page.goto('/edge/tunnels/b111e10396903a33c8edf38dbcd9eb8f')
    await expect(page.getByRole('region', { name: 'Connector', exact: true })).toContainText('active, not ready, Cloudflare refuses its token')
    await expect(page.getByRole('region', { name: 'Connectors pco does not run' }).locator('code')).toHaveText('pco tunnel rotate --account b111e10396903a33c8edf38dbcd9eb8f')
    expect(await fake.calls('RotateTunnel'), 'what the pages asked the daemon to rotate').toEqual([])
  })
})
