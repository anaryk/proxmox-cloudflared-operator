import type { BrowserContext, Page } from '@playwright/test'

import { admin, expect, type Pco, populated, statusOf, test, withTicket } from './fixtures'

// How live the page says its data is, against the stream of pco web and the
// cycles of the daemon, which the controls of the fake daemon stop, drop and
// restart. A cycle is stale after three times the larger of the poll
// interval and the last cycle's length, as pco doctor's cycle check has it.

const overview = (page: Page) => page.getByRole('heading', { level: 1, name: 'Overview' })
const banner = (page: Page, text: string) => page.getByRole('alert').filter({ hasText: text })

async function signedIn(page: Page, context: BrowserContext, pco: Pco): Promise<void> {
  await withTicket(context, pco, admin)
  await page.goto('/')
  await expect(overview(page)).toBeVisible()
  await expect(statusOf(page)).toContainText('Live')
}

// shows waits for the status of the top bar to say text at some moment, even
// for one frame: what passes quickly is missed by a check that comes later.
function shows(page: Page, text: string): Promise<unknown> {
  return page.waitForFunction((t) => document.querySelector('[aria-label="Status of pco"]')?.textContent?.includes(t) ?? false, text, {
    polling: 'raf',
    timeout: 15_000,
  })
}

// The page ages its data on its own clock, from when the news of the last
// cycle reached it. These tests pause the daemon's notices, load the page,
// whose read of the state is that news, and move the page's clock on: the
// few seconds of real time a step takes stay below the margins.

test('cycles that stop make the page stale after three poll intervals', async ({ page, context, pco, fake }) => {
  await page.clock.install()
  await signedIn(page, context, pco)
  // The populated scenario polls every 10 s and its cycles take 2 s.
  await fake.post('/streams/pause')
  await page.reload()
  await expect(statusOf(page)).toContainText('Live')

  await page.clock.fastForward(25_000)
  await expect(statusOf(page)).toContainText(/Live2\d s/)
  await page.clock.fastForward(6_000)
  await expect(statusOf(page)).toContainText('Stale')
  await expect(page.getByText('no cycle of the daemon has finished since')).toBeVisible()
})

test('a cycle of 40 s with a poll interval of 10 s is still live at 100 s', async ({ page, context, pco, fake }) => {
  await page.clock.install()
  await signedIn(page, context, pco)
  await fake.post('/streams/pause')
  const st = populated()
  st.at = new Date(Date.parse(st.finishedAt ?? '') - 40_000).toISOString()
  // The digest of a state leaves its times out: a problem more makes it
  // another state, which pco web reads anew.
  st.problems = [...(st.problems ?? []), 'the cycles take 40 s']
  await fake.post('/state', st)
  await page.reload()
  await expect(page.getByText('the cycles take 40 s')).toBeVisible()

  await page.clock.fastForward(100_000)
  await expect(statusOf(page)).toContainText('Live1 min')
  await page.clock.fastForward(21_000)
  await expect(statusOf(page)).toContainText('Stale')
})

test('a stream of the daemon that drops is followed again, the page holding its own', async ({ page, context, pco, fake }) => {
  const streams: string[] = []
  context.on('request', (r) => {
    if (new URL(r.url()).pathname === '/api/v1/stream') streams.push(r.url())
  })
  await signedIn(page, context, pco)
  expect(await fake.subscribers()).toBe(1)

  // pco web tells the page that the daemon went, and that it is back once
  // it follows the daemon's stream again.
  const away = shows(page, 'no answer')
  await fake.post('/streams/drop')
  await away
  await expect(statusOf(page)).toContainText('Live')
  await expect.poll(() => fake.subscribers()).toBe(1)
  expect(streams, 'the streams the page opened to pco web').toHaveLength(1)
})

test('a daemon that is gone shows the banner of the daemon', async ({ page, context, pco, fake, watch }) => {
  // The browser's lines for the page's reads that pco web answers with 502.
  watch.allow(/^console error on \S+: Failed to load resource: the server responded with a status of 502\b/)
  await signedIn(page, context, pco)
  await fake.kill()
  await expect(banner(page, 'The pco daemon does not answer (since')).toContainText('Published routes keep working; nothing changes until it is back.')
  await expect(statusOf(page)).toContainText('Daemonno answer')
})

test('pco web that stops shows the banner of the web process', async ({ page, context, pco, watch }) => {
  // The browser's lines for the page's tries to reach it again.
  watch.allow(/^console error on \S+: Failed to load resource: /)
  await signedIn(page, context, pco)
  const reconnecting = shows(page, 'Reconnecting')
  await pco.stop()
  await reconnecting
  await expect(banner(page, "This page cannot reach pco's web process (since")).toContainText('Check that pco-web.service runs on the node.')
  await expect(statusOf(page)).toContainText('pco webno answer')

  // Started again, it holds no session; the page signs in with the
  // session of Proxmox VE by itself and follows it again.
  await pco.start()
  await expect(statusOf(page)).toContainText('Live', { timeout: 20_000 })
  await expect(banner(page, "This page cannot reach pco's web process")).toHaveCount(0)
  await expect(page.getByRole('dialog', { name: 'Your session has ended' })).toHaveCount(0)
})

test('a new boot of the daemon resets what the page holds', async ({ page, context, pco, fake }) => {
  await signedIn(page, context, pco)
  await fake.post('/event', { level: 'info', kind: 'egress', subject: 'egress', message: 'an event of the boot before' })
  await expect(page.getByText('an event of the boot before').first()).toBeVisible()

  const reads: (string | undefined)[] = []
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/api/v1/state') reads.push(r.headers()['if-none-match'])
  })
  await fake.post('/boot')
  await expect(page.getByText('No events yet').first()).toBeVisible()
  await expect(page.getByText('an event of the boot before')).toHaveCount(0)
  expect(reads, 'the state read whole, not checked against the one held').toContain(undefined)
  await expect(statusOf(page)).toContainText('Live')
  await expect(overview(page)).toBeVisible()
})

test('two tabs share one stream, and the other takes it over when the first closes', async ({ page, context, pco, fake, browserName }) => {
  const opened: Page[] = []
  context.on('request', (r) => {
    if (new URL(r.url()).pathname === '/api/v1/stream') opened.push(r.frame().page())
  })
  await signedIn(page, context, pco)
  const second = await context.newPage()
  if (browserName === 'firefox') {
    // Playwright's Firefox now and then loses the navigation of a second
    // page that loads from the cache; a route keeps it off the cache.
    await second.route('**/*', (r) => r.continue())
  }
  await second.goto('/')
  await expect(overview(second)).toBeVisible()
  await expect(statusOf(second)).toContainText('Live')
  // Long enough for a stream of its own to show, would it open one.
  await second.waitForTimeout(2000)
  expect(opened, 'the pages that opened a stream').toEqual([page])
  expect(await fake.subscribers()).toBe(1)

  await page.close()
  await expect.poll(() => opened.length).toBe(2)
  expect(opened[1]).toBe(second)
  await expect(statusOf(second)).toContainText('Live')
  expect(await fake.subscribers()).toBe(1)
})
