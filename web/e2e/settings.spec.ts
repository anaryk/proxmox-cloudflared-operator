import { readFileSync } from 'node:fs'

import type { Page } from '@playwright/test'

import { csrfOf, expect, type FakeDaemon, signIn, test } from './fixtures'

// The settings of the populated scenario: a save shows its changes and sends
// them with the revision the page read, a save that crossed another one
// shows both, a link from a rejected route adds its pattern and saves
// nothing by itself, and a file is exported and imported, checked before
// anything is written.

interface Settings {
  rev: number
  settings: Record<string, unknown>
}

interface Saved {
  rev: number
  settings: Record<string, unknown>
}

async function settingsOf(page: Page): Promise<Settings> {
  return (await (await page.request.get('/api/v1/settings')).json()) as Settings
}

async function saves(fake: FakeDaemon): Promise<Saved[]> {
  return (await fake.calls('SaveSettings')).map((c) => c.args as Saved)
}

const field = (page: Page, name: string) => page.getByRole('form', { name: 'Settings' }).getByRole('textbox', { name, exact: true })

test('a change is shown before it is saved, and saved at the revision the page read', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/settings')
  const before = await settingsOf(page)
  await fake.clearCalls()
  await field(page, 'Poll interval').fill('15s')
  await field(page, 'Cloudflare budget').fill('900')
  await page.getByRole('button', { name: 'Save', exact: true }).click()

  const review = page.getByRole('dialog', { name: 'Save these changes?' })
  await expect(review).toContainText('pollInterval')
  await expect(review).toContainText('15s')
  await expect(review).toContainText('cloudflareBudget is read only when pco starts: it takes effect after systemctl restart pco.')
  expect(await saves(fake)).toEqual([])
  await review.getByRole('button', { name: 'Save settings' }).click()

  await expect(page.getByText(`The settings are saved; they are at revision ${before.rev + 1}.`)).toBeVisible()
  const sent = await saves(fake)
  expect(sent).toHaveLength(1)
  expect(sent[0]?.rev).toBe(before.rev)
  expect(sent[0]?.settings).toEqual({ ...before.settings, pollInterval: '15s', cloudflareBudget: 900 })
})

test('a save that crossed another shows both, and keeps the edits on the saved settings', async ({ page, pco, fake, watch }) => {
  // the browser's line for the answer of 409
  watch.allow(/^console error on \S+\/settings: Failed to load resource: the server responded with a status of 409\b/)
  await signIn(page, pco, '/settings')
  const before = await settingsOf(page)
  await field(page, 'Grace').fill('2m0s')

  // Another admin saves the poll interval meanwhile.
  const theirs = await page.request.put('/api/v1/settings', {
    data: { rev: before.rev, settings: { ...before.settings, pollInterval: '20s' } },
    headers: { Origin: pco.url, 'Pco-Csrf': await csrfOf(page) },
  })
  expect(theirs.status()).toBe(200)
  await fake.clearCalls()

  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByRole('dialog', { name: 'Save these changes?' }).getByRole('button', { name: 'Save settings' }).click()
  const conflict = page.getByRole('dialog', { name: 'The settings changed while you edited' })
  await expect(conflict.getByRole('heading', { name: `Saved by someone else, at revision ${before.rev + 1}` })).toBeVisible()
  await expect(conflict).toContainText('pollInterval')
  await expect(conflict).toContainText('20s')
  await expect(conflict).toContainText('grace')
  await expect(conflict).toContainText('2m0s')
  await conflict.getByRole('button', { name: 'Keep my edits' }).click()
  await expect(field(page, 'Poll interval')).toHaveValue('20s')
  await expect(field(page, 'Grace')).toHaveValue('2m0s')

  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByRole('dialog', { name: 'Save these changes?' }).getByRole('button', { name: 'Save settings' }).click()
  await expect(page.getByText(`The settings are saved; they are at revision ${before.rev + 2}.`)).toBeVisible()
  const sent = await saves(fake)
  expect(sent.map((s) => s.rev)).toEqual([before.rev, before.rev + 1])
  expect(sent[1]?.settings).toEqual({ ...before.settings, pollInterval: '20s', grace: '2m0s' })
})

test('a link from a rejected route adds its pattern and says what it grants, and Save saves it', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/settings?addAllowHost=*.store.example.com&owner=lxc%2F210')
  await fake.clearCalls()
  const allowed = page.getByRole('textbox', { name: 'Allowed hostnames' })
  await expect(allowed).toHaveValue('*.store.example.com')
  await expect(allowed).toBeFocused()
  expect(await saves(fake), 'nothing is saved by the link').toEqual([])

  await page.getByRole('button', { name: 'Save', exact: true }).click()
  const review = page.getByRole('dialog', { name: 'Save these changes?' })
  await expect(review).toContainText('*.store.example.com is asked for by lxc/210.')
  await review.getByRole('button', { name: 'Save settings' }).click()
  await expect.poll(async () => (await saves(fake)).map((s) => s.settings.allowHosts)).toEqual([['*.store.example.com']])
})

test('a link that names no rejected route adds nothing', async ({ page, pco }) => {
  await signIn(page, pco, '/settings?addAllowHost=example.com&owner=qemu%2F101')
  await expect(page.getByText('This link names no rejected route, so nothing was added to the allowed hostnames.')).toBeVisible()
  await expect(page.getByRole('textbox', { name: 'Allowed hostnames' })).toHaveValue('')
  await expect(page.getByRole('button', { name: 'Save', exact: true })).toBeDisabled()
})

test('the settings and the manual routes go out as a file and come back checked', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/settings')
  const downloading = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Export' }).click()
  const download = await downloading
  // named by the node pco web runs on, which is this machine
  const { node } = (await (await page.request.get('/api/session')).json()) as { node: string }
  expect(download.suggestedFilename()).toMatch(new RegExp(`^pco-settings-${node.replace(/[.]/g, '\\.')}-\\d{8}\\.json$`))
  const exported = JSON.parse(readFileSync((await download.path()) ?? '', 'utf8')) as { rev: number; settings: Record<string, unknown>; manualRoutes: { id: string }[] }
  expect(Object.keys(exported).sort()).toEqual(['manualRoutes', 'rev', 'settings'])
  expect(exported.manualRoutes.map((r) => r.id)).toEqual(['status'])
  await fake.clearCalls()

  const file = page.getByLabel('Settings file')
  const broken = { ...exported, settings: { ...exported.settings, pollInterval: '1s' } }
  await file.setInputFiles({ name: 'broken.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(broken)) })
  const refused = page.getByRole('dialog', { name: 'This file cannot be imported' })
  await expect(refused).toContainText('pollInterval')
  await refused.getByRole('button', { name: 'Close' }).last().click()

  const changed = {
    ...exported,
    settings: { ...exported.settings, grace: '2m0s' },
    manualRoutes: [...exported.manualRoutes, { id: 'docs', hostname: 'docs.example.com', target: { kind: 'address', scheme: 'http', addr: '10.0.5.21', port: 8080 }, options: {} }],
  }
  await file.setInputFiles({ name: 'changed.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(changed)) })
  const review = page.getByRole('dialog', { name: 'Import this file?' })
  await expect(review).toContainText('The file passed every check. Nothing has been written yet.')
  await expect(review).toContainText('grace')
  await expect(review.getByRole('heading', { name: 'Added (1)' })).toBeVisible()
  await expect(review).toContainText('docs: ')
  expect(await fake.calls('SaveSettings', 'CreateManualRoute', 'UpdateManualRoute', 'DeleteManualRoute'), 'nothing written before the import').toEqual([])

  await review.getByRole('button', { name: 'Import' }).click()
  await expect(page.getByRole('dialog', { name: 'Import', exact: true })).toBeVisible()
  const written = await fake.calls('SaveSettings', 'CreateManualRoute', 'UpdateManualRoute', 'DeleteManualRoute')
  expect(written.map((c) => c.method)).toEqual(['SaveSettings', 'CreateManualRoute'])
  expect((written[0]?.args as Saved).settings.grace).toBe('2m0s')
})
