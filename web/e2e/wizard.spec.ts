import type { Page } from '@playwright/test'

import type { RouteView } from '../src/api/types.gen'
import { expect, populated, reader, signIn, stateOf, test } from './fixtures'

// The first-run setup past the token: the install check that only root on
// the node can satisfy, the first route as a block for the Notes that the
// step then follows until it serves, and publishing started from the last
// step. setup.spec.ts has the token and the zones.

const step = (page: Page, name: RegExp) => page.getByRole('button', { name })
const region = (page: Page, name: RegExp) => page.getByRole('region', { name })

test.describe('a node whose install is not finished', () => {
  test.use({ scenario: 'hold' })

  test('the setup gives the command for root, and looks again after every cycle', async ({ page, pco, fake }) => {
    await signIn(page, pco, '/setup')
    const install = region(page, /: Install check/)
    await expect(install).toContainText('no writer identity; run pco setup')
    await expect(install.locator('code')).toHaveText('pco setup')
    await expect(install.getByRole('button', { name: 'Copy' })).toBeVisible()

    const st = stateOf('hold')
    st.problems = st.problems.filter((p) => !p.includes('pco setup'))
    st.hold = undefined
    await fake.post('/state', st)
    await expect(step(page, /: Install check/)).toHaveCount(0)
    await expect(step(page, /^Step 1: API token/)).toBeVisible()
  })
})

test.describe('a new install with a token', () => {
  test.use({ scenario: 'first-run' })

  // The state after the cycles that read the token and its zones: observe
  // only, no route yet.
  function noRoute() {
    const st = populated()
    st.mode = 'observe'
    st.routes = []
    st.waiting = []
    st.offer = ''
    st.unapproved = []
    st.problems = []
    return st
  }

  const route = (state: string, reason?: string): RouteView => ({
    hostname: 'docs.example.com',
    owner: 'qemu/140',
    guest: { kind: 'qemu', vmid: 140, name: 'docs' },
    state,
    ...(reason ? { reason } : {}),
    replicas: [],
    zone: 'example.com',
    accountId: 'acc1',
    ...(state === 'active' ? { service: 'http://10.0.0.140:8080', level: 'port' } : {}),
  })

  test('the first route is a block for the Notes, followed until it serves', async ({ page, pco, fake }) => {
    await fake.post('/state', noRoute())
    await signIn(page, pco, '/setup')
    // the step is optional: publishing is what is left to do, and the route
    // step opens when it is chosen
    await expect(step(page, /^Step 5: Start publishing/)).toHaveAttribute('aria-expanded', 'true')
    await step(page, /^Step 4: First route/).click()
    await expect(step(page, /^Step 4: First route/)).toHaveAttribute('aria-expanded', 'true')
    const annotate = page.getByRole('region', { name: 'Annotate a guest' })
    await annotate.getByRole('textbox', { name: 'Name' }).fill('docs')
    await annotate.getByRole('textbox', { name: 'Port' }).fill('8080')
    const block = annotate.getByLabel('The block for the Notes')
    await expect(block).toContainText('docs.example.com')
    await expect(block).toContainText('8080')
    await expect(annotate.getByRole('status').filter({ hasText: 'Waiting for a route for docs.example.com' })).toBeVisible()

    // The next cycles read the Notes: the route is there, then it serves.
    const st = noRoute()
    st.routes = [route('unreachable', 'no verified address yet')]
    await fake.post('/state', st)
    const watched = annotate.locator('.route-watch')
    await expect(watched).toContainText('docs.example.com')
    await expect(watched).toContainText('unreachable')
    await expect(watched).toContainText('no verified address yet')
    await expect(watched.getByRole('link', { name: 'Diagnose it' })).toHaveAttribute('href', '/routes/docs.example.com?owner=qemu%2F140')

    st.routes = [route('active')]
    await fake.post('/state', st)
    await expect(watched).toContainText('active')
    await expect(watched).not.toContainText('no verified address yet')
  })

  test('publishing is started from the last step, and says so', async ({ page, pco, fake }) => {
    const st = noRoute()
    st.routes = [route('active')]
    await fake.post('/state', st)
    await signIn(page, pco, '/setup')
    // the step that is left to do is open
    await expect(step(page, /^Step 5: Start publishing/)).toHaveAttribute('aria-expanded', 'true')
    const publish = region(page, /^Step 5: Start publishing/)
    await expect(publish).toContainText('pco is observing only: it changes nothing at Cloudflare until you start publishing.')
    await fake.clearCalls()
    await publish.getByRole('button', { name: 'Start publishing' }).click()
    await expect(publish.getByRole('status')).toContainText('Publishing has started: the daemon changes Cloudflare from the next cycle.')
    expect((await fake.calls('Apply')).map((c) => c.args)).toEqual([{ confirmDeletes: false, offer: '' }])
  })

  test('publishing starts before any route, with the segment of the appliance acknowledged in the same click', async ({ page, pco, fake }) => {
    const st = noRoute()
    st.segments = [{ bridge: 'vmbr0', acknowledged: false, routes: 0, own: true }]
    await fake.post('/state', st)
    await signIn(page, pco, '/setup')
    await expect(step(page, /^Step 5: Start publishing/)).toHaveAttribute('aria-expanded', 'true')
    const publish = region(page, /^Step 5: Start publishing/)
    await expect(publish.getByRole('checkbox', { name: "vmbr0 (an appliance's card)" })).toBeChecked()
    await fake.clearCalls()
    await publish.getByRole('button', { name: 'Start publishing' }).click()
    await expect(publish.getByRole('status')).toContainText('Publishing has started')
    expect((await fake.calls('AcknowledgeSegment')).map((c) => c.args)).toEqual([{ bridge: 'vmbr0', vlan: 0 }])
    expect((await fake.calls('Apply')).map((c) => c.args)).toEqual([{ confirmDeletes: false, offer: '' }])
  })

  test('a reader is told that an admin has to finish the setup', async ({ page, pco }) => {
    await signIn(page, pco, '/setup', reader)
    await expect(page.getByText('An admin has to finish the setup')).toBeVisible()
    await expect(page.getByRole('textbox', { name: 'Cloudflare API token' })).toHaveCount(0)
  })
})
