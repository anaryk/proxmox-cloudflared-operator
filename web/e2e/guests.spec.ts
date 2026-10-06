import type { Page } from '@playwright/test'

import { expect, populated, rowsOf, signIn, test } from './fixtures'

// Approvals, revocations and claims against the populated scenario: each
// dialog repeats what it does and sends exactly that, a guest that changed
// since is refused in the daemon's words, and no guest can be approved while
// the last cycle did not list them all.

const approveDialog = (page: Page) => page.getByRole('dialog', { name: 'Approve a guest' })
const waiting = (page: Page) => page.getByRole('table', { name: 'Guests waiting for approval' })

test('approving a guest that waits sends the identity, MACs and addresses it showed', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/guests')
  await fake.clearCalls()
  await rowsOf(waiting(page)).filter({ hasText: 'lxc/202' }).getByRole('button', { name: 'Approve' }).click()
  const dialog = approveDialog(page)
  await expect(dialog).toContainText('lxc/202 (dns-1) waits for approval in identity uuid:202; approved, it publishes dns.example.com.')
  await expect(dialog).toContainText('Approving it records MAC bc:24:11:00:02:02 and allows address 10.0.0.1.')
  await expect(dialog.getByRole('listitem')).toHaveText(['delegated: alice@pve holds VM.Config.Network', 'address 10.0.0.1 is the gateway of node pve1'])
  expect(await fake.calls('ApproveGuest')).toEqual([])

  await dialog.getByRole('button', { name: 'Approve' }).click()
  await expect(page.getByText(/^Approved lxc\/202 \(dns-1\) in identity uuid:202\. From the next cycle its routes no longer wait for an approval\.$/)).toBeVisible()
  await expect(dialog).toHaveCount(0)
  expect((await fake.calls('ApproveGuest')).map((c) => c.args)).toEqual([{ owner: 'lxc/202', identity: 'uuid:202', macs: ['bc:24:11:00:02:02'], addresses: ['10.0.0.1'] }])
})

test('a guest that changed since it was shown is refused, and the dialog stays', async ({ page, pco, fake, watch }) => {
  // the browser's line for the answer of 409
  watch.allow(/^console error on \S+\/guests: Failed to load resource: the server responded with a status of 409\b/)
  await signIn(page, pco, '/guests')
  const sentence = 'lxc/201 changed since it was shown: its identity is now uuid:9201; look again'
  await fake.post('/refuse', { method: 'ApproveGuest', code: 'refused', message: sentence })
  await rowsOf(waiting(page)).filter({ hasText: 'lxc/201' }).getByRole('button', { name: 'Approve' }).click()
  const dialog = approveDialog(page)
  await dialog.getByRole('button', { name: 'Approve' }).click()
  await expect(dialog).toContainText(sentence)
  await expect(dialog.getByRole('button', { name: 'Look again' })).toBeVisible()
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog).toHaveCount(0)
})

test('no guest can be approved while the last cycle did not list them all', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/guests')
  await fake.post('/state', { ...populated(), complete: false })
  await expect(page.getByRole('group', { name: 'Status of pco' })).toContainText(/incomplete/)
  for (const table of ['Guests waiting for approval', 'All tagged guests']) {
    const buttons = page.getByRole('table', { name: table }).getByRole('button', { name: 'Approve' })
    await expect(buttons.first()).toBeDisabled()
    expect(await buttons.count(), table).toBeGreaterThan(0)
    for (const b of await buttons.all()) await expect(b).toBeDisabled()
  }
  await expect(waiting(page).getByText('the last cycle did not list every guest').first()).toBeVisible()
  await page.goto('/guests/qemu/105')
  await expect(page.getByRole('button', { name: 'Approve' })).toBeDisabled()
  expect(await fake.calls('ApproveGuest')).toEqual([])
})

test('a listed guest that does not wait can be approved from its page', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/guests/qemu/105')
  await fake.clearCalls()
  await page.getByRole('button', { name: 'Approve' }).click()
  const dialog = approveDialog(page)
  await expect(dialog).toContainText('qemu/105 (monitor) has identity uuid:105 in the last listing, and is approved in it')
  await dialog.getByRole('button', { name: 'Approve' }).click()
  await expect(dialog).toHaveCount(0)
  expect((await fake.calls('ApproveGuest')).map((c) => c.args)).toEqual([{ owner: 'qemu/105', identity: 'uuid:105', macs: null, addresses: null }])
})

test('revoking an approval asks first', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/guests')
  await fake.clearCalls()
  await rowsOf(page.getByRole('table', { name: 'Approved guests' })).filter({ hasText: 'qemu/101' }).getByRole('button', { name: 'Revoke' }).click()
  const dialog = page.getByRole('dialog', { name: 'Revoke an approval' })
  await expect(dialog).toContainText('qemu/101')
  expect(await fake.calls('RevokeGuest')).toEqual([])
  await dialog.getByRole('button', { name: 'Revoke' }).click()
  await expect(page.getByText('Revoked the approval of qemu/101 (web-1).')).toBeVisible()
  expect((await fake.calls('RevokeGuest')).map((c) => c.args)).toEqual([{ owner: 'qemu/101' }])
})

test('a hostname in conflict is handed to the owner that waits for it', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/guests/claims')
  await fake.clearCalls()
  await rowsOf(page.getByRole('table', { name: 'Claims' })).filter({ hasText: 'www.example.com' }).getByRole('button', { name: 'Hand to …' }).click()
  const dialog = page.getByRole('dialog', { name: 'Hand a hostname to another owner' })
  await expect(dialog).toContainText('www.example.com is held by qemu/101 (web-1).')
  await expect(dialog.getByRole('button', { name: 'Hand it over' })).toBeDisabled()
  await dialog.getByRole('radio', { name: /^qemu\/102/ }).check()
  await dialog.getByRole('button', { name: 'Hand it over' }).click()
  await expect(dialog).toHaveCount(0)
  expect((await fake.calls('ResolveClaim')).map((c) => c.args)).toEqual([{ hostname: 'www.example.com', owner: 'qemu/102' }])
})

test('an issue in the Notes of a guest is shown where it is', async ({ page, pco, fake }) => {
  const st = populated()
  st.issues = [{ guest: { kind: 'qemu', vmid: 103 }, line: 5, col: 20, msg: 'a port is a number' }]
  await fake.post('/state', st)
  await signIn(page, pco, '/guests')
  const issue = rowsOf(page.getByRole('table', { name: 'Annotation issues' })).filter({ hasText: 'a port is a number' })
  await expect(issue).toContainText('qemu/103')
  await expect(issue.getByRole('cell')).toHaveText(['qemu/103', '5', '20', 'a port is a number'])

  await page.goto('/guests/qemu/103')
  const block = page.getByRole('region', { name: 'The route text of the Notes of qemu/103, from line 4' })
  await expect(block.locator('.annotation-line')).toHaveText(['4```cf-tunnel', '5app.example.com -> :3000', '6```'])
  await expect(block.locator('.annotation-caret')).toHaveText(`${' '.repeat(19)}^`)
  await expect(block.locator('.annotation-msg')).toHaveText('line 5, column 20: a port is a number')
})
