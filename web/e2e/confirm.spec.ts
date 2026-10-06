import type { Page } from '@playwright/test'

import { expect, populated, signIn, test } from './fixtures'

// Confirming what waits, as pco apply --confirm-deletes: the dialog sends
// the offer of the list it showed and nothing else, a list that changes
// while it is open is reviewed again before anything is sent, and a refusal
// of the daemon says why. Adopting a record of someone else asks first too.

const dialog = (page: Page) => page.getByRole('dialog', { name: 'Confirm what waits' })
const accept = (page: Page) => dialog(page).getByRole('button', { name: /^Accept .* at the next run$/ })
const typed = (page: Page) => dialog(page).getByRole('textbox', { name: 'Type confirm to accept the removals' })

async function openConfirmation(page: Page): Promise<string> {
  await page.getByRole('region', { name: 'Waits for a confirmation' }).getByRole('button', { name: 'Review and confirm' }).click()
  await expect(dialog(page)).toBeVisible()
  const offer = (await dialog(page).locator('.offer .mono').innerText()).trim()
  expect(offer).toMatch(/^[0-9a-f]{16}$/)
  return offer
}

test('the confirmation sends the offer of the list it showed', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/routes/plan')
  const offer = await openConfirmation(page)
  const served = (await (await page.request.get('/api/v1/state')).json()) as { offer: string }
  expect(offer).toBe(served.offer)
  await expect(dialog(page).locator('.waiting-list > li')).toHaveCount(4)
  await expect(dialog(page).locator('.waiting-items li')).toHaveText(['a.example.com', 'b.example.com', 'qemu/104 db-1', 'lxc/200'])
  await expect(accept(page)).toHaveText('Accept 2 removals, 2 vanished guests, 1 zone that is gone and 1 tunnel that is gone at the next run')
  await expect(accept(page)).toBeDisabled()
  await fake.clearCalls()

  await typed(page).fill('confirm')
  await accept(page).click()
  await expect(dialog(page)).toContainText('Confirmed for the next run:')
  await expect(dialog(page).getByRole('listitem')).toHaveCount(4)
  expect((await fake.calls('Apply')).map((c) => c.args)).toEqual([{ confirmDeletes: true, offer }])
  await dialog(page).getByRole('button', { name: 'Close' }).last().click()
  await expect(dialog(page)).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Waits for a confirmation' })).toHaveCount(0)
})

test('a list that changes while the dialog is open is reviewed before anything is sent', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/routes/plan')
  const first = await openConfirmation(page)
  await typed(page).fill('confirm')
  await expect(accept(page)).toBeEnabled()
  await fake.clearCalls()

  // A cycle finds the tunnel again and another guest gone.
  const st = populated()
  const next = '0f1e2d3c4b5a6978'
  st.offer = next
  st.waiting = st.waiting
    .filter((w) => w.kind !== 'unseen-tunnel')
    .map((w) => (w.kind === 'vanished-guests' ? { ...w, detail: w.detail.replace('2 guests', '3 guests'), items: [...w.items, 'qemu/107 cache-1'] } : w))
  await fake.post('/state', st)

  const alert = dialog(page).getByRole('alert')
  await expect(alert).toContainText('What waits changed while you were reading.')
  await expect(dialog(page).locator('.diff-added li')).toContainText([/3 guests that hold a hostname are no longer listed/])
  await expect(dialog(page).locator('.diff-removed li')).toContainText([/tunnel pco-abc123 .* is not visible through any credential/, /2 guests that hold a hostname/])
  await expect(accept(page)).toHaveCount(0)
  expect(await fake.calls('Apply'), 'nothing was sent').toEqual([])

  await dialog(page).getByRole('button', { name: 'Review the new list' }).click()
  await expect(dialog(page).locator('.offer .mono')).toHaveText(next)
  await expect(dialog(page).locator('.waiting-items li')).toHaveText(['a.example.com', 'b.example.com', 'qemu/104 db-1', 'lxc/200', 'qemu/107 cache-1'])
  await expect(typed(page)).toHaveValue('')
  await typed(page).fill('confirm')
  await accept(page).click()
  await expect(dialog(page)).toContainText('Confirmed for the next run:')
  const sent = (await fake.calls('Apply')).map((c) => c.args)
  expect(sent).toEqual([{ confirmDeletes: true, offer: next }])
  expect(first).not.toBe(next)
})

test('a refusal of the daemon is said in its words, and the list is reviewed again', async ({ page, pco, fake, watch }) => {
  // the browser's line for the answer of 409
  watch.allow(/^console error on \S+\/routes\/plan: Failed to load resource: the server responded with a status of 409\b/)
  await signIn(page, pco, '/routes/plan')
  const offer = await openConfirmation(page)
  const sentence = 'what waits for a confirmation changed since it was shown; look again and repeat'
  await fake.post('/refuse', { method: 'Apply', code: 'refused', message: sentence })
  await fake.clearCalls()

  await typed(page).fill('confirm')
  await accept(page).click()
  await expect(dialog(page).getByRole('alert')).toContainText('What waits changed while you were reading.')
  await expect(dialog(page)).toContainText(`The daemon says: ${sentence}`)
  expect((await fake.calls('Apply')).map((c) => c.args)).toEqual([{ confirmDeletes: true, offer }])

  await dialog(page).getByRole('button', { name: 'Review the new list' }).click()
  await expect(accept(page)).toBeVisible()
  await dialog(page).getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog(page)).toHaveCount(0)
})

test('adopting a record of someone else names it, and asks first', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/routes/plan')
  await fake.clearCalls()
  const way = page.getByRole('region', { name: 'Records of someone else that stand in the way' })
  await way.getByRole('button', { name: 'Adopt' }).click()
  const adopt = page.getByRole('dialog')
  await expect(adopt).toContainText('api.example.com')
  await expect(adopt).toContainText('A 192.0.2.10')
  await expect(adopt).toContainText('adopted.jsonl')
  expect(await fake.calls('Adopt')).toEqual([])
  await adopt.getByRole('button', { name: /^Adopt/ }).last().click()
  await expect.poll(async () => (await fake.calls('Adopt')).map((c) => c.args)).toEqual([{ name: 'api.example.com' }])
})
