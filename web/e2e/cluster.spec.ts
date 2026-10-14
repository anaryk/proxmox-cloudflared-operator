import { createHash } from 'node:crypto'

import { expect, reader, signIn, test } from './fixtures'

// A cluster of three nodes, as the cluster scenario of the fake daemon has
// it: pve1 the controller, pve2 a host node with a tunnel pinned to it, and
// pve3 an appliance agent that waits for admission. The banners say what
// the cluster went through, the nodes have a page each, and a route says
// where it is placed and how each replica reaches it.

test.use({ scenario: 'cluster' })

// The fingerprint the scenario enrolled a node's certificate under.
const enrolled = (node: string) => createHash('sha256').update(node).digest('hex')

test('the banners of the cluster, and its nodes in the navigation', async ({ page, pco, viewport }) => {
  await signIn(page, pco, '/')
  const banners = page.locator('.banners')
  await expect(banners).toContainText('The controller moved from pve2 to pve1, generation 7')
  await expect(banners).toContainText('10.0.0.60 is blocked on every node')
  if ((viewport?.width ?? 0) < 720) await page.getByRole('button', { name: 'Open navigation' }).click()
  const nav = page.getByRole('navigation').first()
  await nav.getByRole('link', { name: /^Nodes/ }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Nodes' })).toBeVisible()
  const table = page.getByRole('table', { name: 'Nodes' })
  await expect(table.getByRole('link', { name: 'pve1' })).toBeVisible()
  await expect(table.getByRole('link', { name: 'pve2' })).toBeVisible()
  await expect(table.getByRole('link', { name: 'pve3' })).toBeVisible()
})

test('an appliance agent is admitted with the fingerprint of its certificate, and a wrong one is refused', async ({ page, pco, fake, watch }) => {
  watch.allow(/^console error on \S+\/nodes\/pve3: Failed to load resource: the server responded with a status of 409\b/)
  await signIn(page, pco, '/nodes/pve3')
  await fake.clearCalls()
  await page.getByRole('button', { name: 'Admit…' }).click()
  const dialog = page.getByRole('dialog', { name: 'Admit a node' })
  await expect(dialog.getByRole('list', { name: 'Uses of the join token' })).toContainText('from 10.0.0.93, as container 9301')
  const field = dialog.getByRole('textbox', { name: 'Fingerprint of the certificate' })

  await field.fill('not hex')
  await dialog.getByRole('button', { name: 'Admit' }).click()
  await expect(dialog).toContainText('digits 0-9 and a-f only')
  expect(await fake.calls('AdmitNode')).toEqual([])

  await field.fill('0123abcd')
  await dialog.getByRole('button', { name: 'Admit' }).click()
  await expect(dialog).toContainText('the fingerprint 0123abcd is not that of the certificate enrolled for node pve3')

  await field.fill(enrolled('pve3').slice(0, 16).toUpperCase())
  await dialog.getByRole('button', { name: 'Admit' }).click()
  await expect(dialog).toHaveCount(0)
  expect((await fake.calls('AdmitNode')).map((c) => c.args)).toEqual([
    { node: 'pve3', fingerprint: '0123abcd', renew: false },
    { node: 'pve3', fingerprint: enrolled('pve3').slice(0, 16), renew: false },
  ])
})

test('forgetting a node needs its name typed, and names the commands for a disk out of your hands', async ({ page, pco, fake }) => {
  await signIn(page, pco, '/nodes/pve2')
  await fake.clearCalls()
  await page.getByRole('button', { name: 'Forget…' }).click()
  const dialog = page.getByRole('dialog', { name: 'Forget a node' })
  await dialog.getByText('The disk of the node is out of your hands').click()
  await expect(dialog.getByText('pvecm delnode pve2', { exact: true })).toBeVisible()
  await expect(dialog.getByText('pco node forget pve2 --compromised', { exact: true })).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Forget' })).toHaveAttribute('aria-disabled', 'true')

  await dialog.getByRole('textbox', { name: 'Type the name of the node to confirm' }).fill('pve2')
  await dialog.getByRole('button', { name: 'Forget' }).click()
  await expect(dialog.getByRole('list', { name: 'Done' })).toContainText('its pinned tunnels are deleted in the next cycle')
  expect((await fake.calls('ForgetNode')).map((c) => c.args)).toEqual([{ node: 'pve2', compromised: false }])
})

test('a route says where it is placed, a move under way, and how each replica reaches it', async ({ page, pco }) => {
  await signIn(page, pco, '/routes/grafana.example.com?owner=lxc%2F205')
  const detail = (term: string) => page.locator('dt', { hasText: new RegExp(`^${term}$`) }).locator('xpath=following-sibling::dd[1]')
  await expect(detail('Placement')).toHaveText('shared tunnel pco-abc123')
  await expect(detail('Move')).toContainText('moving from pco-abc123-pve2 to pco-abc123')
  const replicas = page.getByRole('list', { name: 'Replicas' })
  await expect(replicas.getByRole('listitem')).toHaveText([/^pve1port \(attested by pve2\)reached, in its set$/, /^pve2portreached, in its set$/])

  await page.goto('/routes/git.example.com?owner=qemu%2F110')
  await expect(detail('Placement')).toContainText('pinned to pve2 (tunnel pco-abc123-pve2)')
  await detail('Tunnel').getByRole('link').click()
  await expect(page).toHaveURL(/\/edge\/tunnels\/acc1\?tunnel=pco-abc123-pve2$/)
  await expect(page.getByRole('region', { name: 'Main · pinned to pve2' })).toBeVisible()
})

test('the map has a replica per node, and a replica opens its node', async ({ page, pco, viewport }) => {
  test.skip((viewport?.width ?? 0) < 720, 'a phone shows the chain list, not the map')
  await signIn(page, pco, '/')
  const map = page.getByRole('group', { name: 'Flow map' })
  await map.getByRole('button', { name: /^Replica pve2 of tunnel Main · 00000000/ }).click()
  const drawer = page.getByRole('complementary')
  await expect(drawer.getByRole('heading').first()).toHaveText('pve2')
  await expect(drawer.getByRole('list', { name: 'Routes pinned to it' })).toContainText('git.example.com')
})

test('the settings show the cluster block as the file has it', async ({ page, pco }) => {
  await signIn(page, pco, '/settings')
  await expect(page.getByRole('region', { name: 'Cluster' })).toContainText('agentNetwork10.10.0.0/24')
})

test('the agent of this node, as an admin opens it from the navigation', async ({ page, pco, fake, viewport }) => {
  await signIn(page, pco, '/nodes')
  await fake.clearCalls()
  if ((viewport?.width ?? 0) < 720) await page.getByRole('button', { name: 'Open navigation' }).click()
  await page.getByRole('navigation').first().getByRole('link', { name: 'This node' }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'This node' })).toBeVisible()
  const card = page.getByRole('region', { name: 'Agent' })
  await expect(card).toContainText('pve1 (this node)')
  await expect(card).toContainText(enrolled('pve1'))
  await expect(card).toContainText('sequence 42')
  expect(await fake.calls('AgentView')).not.toEqual([])
})

test('a reader is not offered the agent of this node, and its page says it is for admins', async ({ page, pco, fake, viewport }) => {
  await signIn(page, pco, '/nodes', reader)
  if ((viewport?.width ?? 0) < 720) await page.getByRole('button', { name: 'Open navigation' }).click()
  const group = page.getByRole('navigation').first().getByRole('group', { name: 'Cluster' })
  await expect(group.getByRole('link', { name: /^Nodes/ })).toBeVisible()
  await expect(group.getByRole('link', { name: 'This node' })).toHaveCount(0)
  await fake.clearCalls()
  await page.goto('/agent')
  await expect(page.getByRole('heading', { level: 1, name: 'This node' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Agent' })).toContainText('This page is for admins')
  await expect(page.getByRole('alert')).toHaveCount(0)
  expect(await fake.calls('AgentView')).toEqual([])
})
