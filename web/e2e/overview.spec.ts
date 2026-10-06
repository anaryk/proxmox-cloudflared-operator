import type { Page } from '@playwright/test'

import { expect, signIn, test } from './fixtures'

// The Overview at the sizes of the fake daemon's large scenarios, which the
// map folds to its budget of 150 cards, and before the first cycle, when
// all there is to say are the problems the daemon started with.

const map = (page: Page) => page.getByRole('group', { name: 'Flow map' })
const cards = (page: Page) => map(page).locator('.fm-card')
const search = (page: Page) => page.getByRole('searchbox', { name: 'Focus a hostname, guest, zone or tunnel' })

const phone = ({ viewport }: { viewport: { width: number } | null }) => (viewport?.width ?? 0) < 720

test.describe('1000 routes and 2000 guests', () => {
  test.use({ scenario: 'large' })

  test('the map stays within its budget, problems first, and a search focuses one chain', async ({ page, pco, viewport }) => {
    test.skip(phone({ viewport }), 'below 720 px the Overview shows the chain list instead of the map')
    await signIn(page, pco)
    await expect(cards(page).first()).toBeVisible()
    const drawn = await cards(page).count()
    expect(drawn, 'cards on the map').toBeLessThanOrEqual(150)
    expect(await map(page).locator('.fm-lines [data-edge]').count(), 'lines on the map').toBeLessThanOrEqual(400)
    await expect(page.getByRole('checkbox', { name: 'Problems first' })).toBeChecked()
    await expect(map(page).getByRole('button', { name: /^\d+ guests on vmbr0, active, opens them$/ })).toBeVisible()

    await search(page).fill('app-0500.example.com')
    await expect(page).toHaveURL(/[?&]focus=app-0500\.example\.com/)
    await expect(map(page).getByRole('button', { name: /^app-0500\.example\.com, active, served by qemu\/\d+ app-0500 on port 8080$/ })).toBeVisible()
    await expect.poll(() => cards(page).count()).toBeLessThan(10)
    await expect(map(page).getByRole('button', { name: /^app-0501\.example/ })).toHaveCount(0)

    await search(page).fill('')
    await expect.poll(() => cards(page).count()).toBe(drawn)
  })

  test('on a phone the chain list holds a screenful of its 1000 hostnames', async ({ page, pco, viewport }) => {
    test.skip(!phone({ viewport }), 'the chain list is the view of a phone')
    await signIn(page, pco)
    const chains = page.getByRole('list', { name: 'Hostnames and their chains' })
    await expect(chains.getByRole('listitem').first()).toBeVisible()
    expect(await chains.getByRole('listitem').count(), 'hostnames in the page').toBeLessThan(100)

    await search(page).fill('app-0500.example.com')
    await expect(chains.getByRole('listitem')).toHaveCount(1)
    await expect(chains.getByRole('button', { name: /app-0500\.example\.com/ })).toBeVisible()
  })
})

test.describe('600 routes down', () => {
  test.use({ scenario: 'outage' })
  test.skip(phone, 'below 720 px the Overview shows the chain list instead of the map')

  test('the routes that are down are grouped by zone, state and reason', async ({ page, pco }) => {
    await signIn(page, pco)
    for (const reason of ['no verified address yet', 'target is not answering', 'address was verified for another owner']) {
      await expect(map(page).getByRole('button', { name: new RegExp(`^\\d+ unreachable: ${reason}, lists their routes$`) }).first()).toBeVisible()
    }
    expect(await cards(page).count()).toBeLessThanOrEqual(150)

    // A group's routes are in the chain list, not on the map.
    await map(page).getByRole('button', { name: /^\d+ unreachable: target is not answering, lists their routes$/ }).first().click()
    const drawer = page.getByRole('complementary')
    await expect(drawer.getByRole('heading', { name: /^\d+ unreachable: target is not answering$/ })).toBeVisible()
    await expect(drawer.getByRole('list', { name: 'Their routes' }).getByRole('listitem').first()).toContainText('unreachable')
  })
})

test.describe('50 zones in 30 accounts', () => {
  test.use({ scenario: 'wide' })
  test.skip(phone, 'below 720 px the Overview shows the chain list instead of the map')

  test('the map fits its width and scrolls down', async ({ page, pco }) => {
    await signIn(page, pco)
    await expect(cards(page).first()).toBeVisible()
    expect(await map(page).locator('.fm-edge').count()).toBeGreaterThan(30)
    const [frame, view] = await Promise.all([map(page).boundingBox(), map(page).locator('.fm-view').boundingBox()])
    expect(frame && view, 'the map and its view').toBeTruthy()
    if (!frame || !view) return
    expect(Math.abs(view.x - frame.x), 'left edge of the view').toBeLessThanOrEqual(1)
    expect(Math.abs(view.width - frame.width), 'width of the view').toBeLessThanOrEqual(1)
    expect(view.height, 'the view is taller than the map shows').toBeGreaterThan(frame.height)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true)
  })
})

test.describe('before the first cycle of a new install', () => {
  test.use({ scenario: 'first-run' })

  test('the Problems card shows the problems the daemon started with', async ({ page, pco }) => {
    await signIn(page, pco)
    const problems = page.getByRole('region', { name: '1 problem' })
    await expect(problems.getByRole('listitem')).toHaveText(['no Cloudflare credential; add one with pco credential add'])
    await expect(page.getByRole('region', { name: 'Flow' })).toContainText('Waiting for the first cycle')
    await expect(page.getByRole('region', { name: 'Flow' }).getByText('no routes', { exact: false })).toHaveCount(0)

    // The problems pill of the top bar leads to the card.
    await page.getByRole('link', { name: '1 problem' }).click()
    await expect(problems).toBeFocused()
  })
})

test('a map whose code does not load says so, and the page asks for a reload', async ({ page, pco, viewport, watch }) => {
  test.skip(phone({ viewport }), 'below 720 px the Overview shows the chain list instead of the map')
  // As after an update of pco: the chunk this page was built with is gone.
  // The browser says so, and React logs what the part caught, which
  // Firefox gives without its message.
  watch.allow(/^console error on \S+: Failed to load resource: the server responded with a status of 404\b/)
  watch.allow(/^console error on \S+: .*(dynamically imported module|Importing a module script failed)/i)
  watch.allow(/^console error on \S+: NotLoaded: a part of the page did not load\b/)
  watch.allow(/^console error on \S+: (Error|JSHandle@object)$/)
  await page.route(/\/assets\/FlowMap-[^/]+\.js$/, (r) => r.continue({ url: new URL('/assets/FlowMap-gone.js', r.request().url()).href }))
  await signIn(page, pco)
  await expect(page.getByRole('region', { name: 'Flow' })).toContainText('The map did not load')
  await expect(page.getByText('pco was updated; reload to use it.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Reload' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Live events' })).toBeVisible()
})
