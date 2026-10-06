// The screenshots of the web interface in docs/images, made by make
// docs-screenshots against the install of scenario.ts: every page and the
// states the pages of docs/ show, each in the light and the dark theme, as
// ui-<page>[-<state>]-<light|dark>.png. A file is written only when what it
// shows changes (png.ts says how much is a change).
//
// The time stands still: the fake daemon's clock at the scenario's moment,
// the page's a few seconds later, and the page's clock moves only as a shot
// moves it, by the same amounts every time. The dots of the flow map are
// where that time puts them.

import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import type { Page } from '@playwright/test'

import type { State } from '../../src/api/types.gen'
import { admin, applianceHarness, expect, type FakeDaemon, type Pco, reader, type User, test, withTicket } from '../fixtures'
import { recompress, samePicture } from './png'
import { firstActions, firstRoute, firstRun, now, observing, pageTime, scenarioDir, setupDir } from './scenario'

const out = process.env.PCO_E2E_SHOTS ?? ''

test.use({ scenario: scenarioDir(), clock: now })

test.beforeEach(async ({ page }) => {
  const at = Date.parse(pageTime)
  await page.clock.install({ time: at - 1000 })
  await page.clock.pauseAt(at)
})

// settle lets the page finish what it does by itself. A part that loads as a
// chunk of its own shows when a timer of React fires, and only the page's
// clock fires it: the clock moves 2 s, in steps that leave the browser time
// to load and draw between them.
async function settle(page: Page): Promise<void> {
  for (let i = 0; i < 25; i++) {
    await page.clock.runFor(80)
    await page.waitForTimeout(100)
  }
}

// open opens path as user, signed in with the session of Proxmox VE, and
// waits until the page shows all of it.
async function open(page: Page, pco: Pco, path: string, user: User = admin): Promise<void> {
  await withTicket(page.context(), pco, user)
  const read = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/v1/state' && r.ok())
  await page.goto(path)
  await read
  await settle(page)
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
  await expect(page.getByRole('main').locator('.skeleton')).toHaveCount(0)
}

// shoot writes the page as it is in both themes, into a file whose picture
// it changes. The pointer is moved where it lights nothing, and the focus a
// dialog or a drawer moved is let go.
async function shoot(page: Page, name: string): Promise<void> {
  await page.mouse.move(1279, 799)
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
  for (const scheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme: scheme })
    const png = recompress(await page.screenshot({ animations: 'disabled' }))
    const file = join(out, `ui-${name}-${scheme}.png`)
    if (!existsSync(file) || !samePicture(readFileSync(file), png)) writeFileSync(file, png)
  }
}

// atMap scrolls the flow map to the top of the window, below the top bar.
async function atMap(page: Page): Promise<void> {
  await page.getByRole('region', { name: 'Flow' }).evaluate((el) => el.scrollIntoView({ block: 'start' }))
}

// openMap opens the overview at path with the map drawn and fitted to its
// width, and then lets its dots move for 1.6 s: the motion is paused while
// the map is drawn, so that they start from the same frame every time.
async function openMap(page: Page, pco: Pco, path: string, { icons = false } = {}): Promise<void> {
  await withTicket(page.context(), pco, admin)
  const read = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/v1/state' && r.ok())
  await page.goto(path)
  await read
  if (icons) await page.getByRole('button', { name: 'Show icons only' }).click()
  await page.getByRole('button', { name: 'Pause motion' }).click()
  await settle(page)
  const map = page.getByRole('group', { name: 'Flow map' })
  await expect(map.locator('.fm-lines .fm-edge-trunk .fm-figure').first()).toBeVisible()
  await page.getByRole('button', { name: 'Fit' }).click()
  await settle(page)
  await page.getByRole('button', { name: 'Resume motion' }).click()
  await page.clock.runFor(1600)
}

// withState serves st before the page is opened: the streams of the fake
// daemon are paused, and the page reads it when it loads.
async function withState(fake: FakeDaemon, st: State): Promise<void> {
  await fake.post('/state', st)
}

test('overview', async ({ page, pco }) => {
  await openMap(page, pco, '/')
  await page.evaluate(() => window.scrollTo(0, 0))
  await shoot(page, 'overview')
})

// The whole map takes more width than the navigation leaves at 1280 px: it
// shows its icons only.
test('the flow map at rest', async ({ page, pco }) => {
  await openMap(page, pco, '/', { icons: true })
  await atMap(page)
  await shoot(page, 'overview-map')
})

test('the flow map drilled into a hostname', async ({ page, pco }) => {
  await openMap(page, pco, '/?focus=hostname:www.example.com', { icons: true })
  await atMap(page)
  await shoot(page, 'overview-focus')
})

test('routes', async ({ page, pco }) => {
  await open(page, pco, '/routes')
  await shoot(page, 'routes')
})

test('a route', async ({ page, pco }) => {
  await open(page, pco, '/routes/www.example.com?owner=qemu%2F101')
  await shoot(page, 'route')
})

test('the plan with changes pending', async ({ page, pco }) => {
  await open(page, pco, '/routes/plan')
  await shoot(page, 'plan')
})

test('a confirmation', async ({ page, pco }) => {
  await open(page, pco, '/routes/plan')
  await page.getByRole('region', { name: 'Waits for a confirmation' }).getByRole('button', { name: 'Review and confirm' }).click()
  const dialog = page.getByRole('dialog', { name: 'Confirm what waits' })
  await expect(dialog).toBeVisible()
  await dialog.getByRole('textbox', { name: 'Type confirm to accept the removals' }).fill('confirm')
  await settle(page)
  await shoot(page, 'plan-confirm')
})

test('a new manual route', async ({ page, pco }) => {
  await open(page, pco, '/routes/manual/new')
  await shoot(page, 'route-manual')
})

test('guests', async ({ page, pco }) => {
  await open(page, pco, '/guests')
  await shoot(page, 'guests')
})

test('guests, as a reader sees them', async ({ page, pco }) => {
  await open(page, pco, '/guests', reader)
  await shoot(page, 'guests-reader')
})

test('a guest', async ({ page, pco }) => {
  await open(page, pco, '/guests/qemu/101')
  await shoot(page, 'guest')
})

test('claims', async ({ page, pco }) => {
  await open(page, pco, '/guests/claims')
  await shoot(page, 'claims')
})

test('networks', async ({ page, pco }) => {
  await open(page, pco, '/networks')
  await shoot(page, 'networks')
})

test('credentials', async ({ page, pco }) => {
  await open(page, pco, '/edge/credentials')
  await shoot(page, 'credentials')
})

test('a credential', async ({ page, pco }) => {
  await open(page, pco, '/edge/credentials/a1b2c3d4')
  await shoot(page, 'credential')
})

test('zones', async ({ page, pco }) => {
  await open(page, pco, '/edge/zones')
  await shoot(page, 'zones')
})

test('tunnels', async ({ page, pco }) => {
  await open(page, pco, '/edge/tunnels')
  await shoot(page, 'tunnels')
})

test('events', async ({ page, pco }) => {
  await open(page, pco, '/events')
  await shoot(page, 'events')
})

test('events filtered', async ({ page, pco }) => {
  await open(page, pco, '/events')
  await page.getByRole('search', { name: 'Filter the events' }).getByRole('checkbox', { name: 'warn' }).check()
  await settle(page)
  await expect(page).toHaveURL(/[?&]level=warn/)
  await shoot(page, 'events-filtered')
})

test('the doctor with a failing check', async ({ page, pco }) => {
  await open(page, pco, '/doctor')
  await page.getByRole('button', { name: 'Run the checks' }).click()
  await settle(page)
  await expect(page.getByRole('region', { name: 'Failures' })).toBeVisible()
  await shoot(page, 'doctor-failing')
})

test('settings', async ({ page, pco }) => {
  await open(page, pco, '/settings')
  await shoot(page, 'settings')
})

test.describe('the first-run setup', () => {
  test.use({ scenario: setupDir() })

  test('the install check', async ({ page, pco, fake }) => {
    const hold = 'no writer identity; run pco setup'
    await withState(fake, { ...firstRun(), problems: [...firstRun().problems, hold], hold })
    await open(page, pco, '/setup')
    await shoot(page, 'setup-install')
  })

  test('the token', async ({ page, pco, fake }) => {
    await withState(fake, firstRun())
    await open(page, pco, '/setup')
    await shoot(page, 'setup-token')
  })

  test('the zones', async ({ page, pco, fake }) => {
    await withState(fake, observing())
    await open(page, pco, '/setup')
    await page.getByRole('button', { name: /^Step 2: Zones/ }).click()
    await settle(page)
    await shoot(page, 'setup-zones')
  })

  test('the first route', async ({ page, pco, fake }) => {
    await withState(fake, observing())
    await open(page, pco, '/setup')
    const annotate = page.getByRole('region', { name: 'Annotate a guest' })
    await annotate.getByRole('textbox', { name: 'Name' }).fill('docs')
    await annotate.getByRole('textbox', { name: 'Port' }).fill('8080')
    await settle(page)
    await expect(annotate.getByLabel('The block for the Notes')).toContainText('docs.example.com')
    await page.getByRole('button', { name: /^Step 4: First route/ }).evaluate((el) => el.scrollIntoView({ block: 'start' }))
    await shoot(page, 'setup-route')
  })

  test('publishing', async ({ page, pco, fake }) => {
    await withState(fake, { ...observing(), routes: [firstRoute], actions: firstActions })
    await open(page, pco, '/setup')
    await expect(page.getByRole('button', { name: /^Step 5: Start publishing/ })).toHaveAttribute('aria-expanded', 'true')
    await shoot(page, 'setup-publish')
  })
})

test('the sign-in', async ({ page }) => {
  await page.goto('/')
  await settle(page)
  await expect(page.getByRole('heading', { level: 1, name: 'Sign in to pco' })).toBeVisible()
  await shoot(page, 'signin')
})

test.describe('in the appliance', () => {
  test.skip(!applianceHarness, 'pco web runs as the appliance only as root on Linux, as in the container of make docs-screenshots')
  test.use({ appliance: true })

  test('the sign-in', async ({ page }) => {
    await page.goto('/')
    await settle(page)
    await expect(page.getByRole('heading', { level: 1, name: 'Sign in to pco' })).toBeVisible()
    await expect(page.getByRole('textbox', { name: 'User' })).toBeVisible()
    await shoot(page, 'signin-appliance')
  })
})
