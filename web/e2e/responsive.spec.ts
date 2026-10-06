import type { Page } from '@playwright/test'

import { expect, type FakeDaemon, populated, signIn, test } from './fixtures'

// Every page reflows to the width of a phone, 375 px, and of a small one,
// 320 px, without a horizontal scroll: what does not fit wraps, or scrolls
// in a box of its own, as a wide table does.

const widths = [375, 320]

// The pages of each scenario: every page of the populated one, and those
// where the other scenarios have their long lines.
const pages: Record<string, string[]> = {
  populated: [
    '/',
    '/?list=1',
    '/routes',
    '/routes/www.example.com?owner=qemu%2F101',
    '/routes/plan',
    '/routes/manual/new',
    '/routes/manual/status',
    '/guests',
    '/guests/qemu/101',
    '/guests/claims',
    '/networks',
    '/edge/credentials',
    '/edge/credentials/cred1',
    '/edge/zones',
    '/edge/zones/example.com',
    '/edge/tunnels',
    '/edge/tunnels/acc1',
    '/events',
    '/doctor',
    '/settings',
    '/setup',
  ],
  rogue: ['/', '/edge/tunnels', '/edge/tunnels/6ab3311bfe7c52d93098e661558a14c2', '/edge/tunnels/b111e10396903a33c8edf38dbcd9eb8f'],
  frozen: ['/', '/routes/plan', '/edge/zones', '/edge/zones/example.shop'],
  'first-run': ['/', '/setup'],
}

// overflow is how far the page is wider than the window, and what sticks
// out of it, for the message of a test that fails.
async function overflow(page: Page): Promise<{ by: number; what: string[] }> {
  return page.evaluate(() => {
    const d = document.documentElement
    const what: string[] = []
    for (const el of document.querySelectorAll('body *')) {
      const r = el.getBoundingClientRect()
      if (r.width > 0 && r.right > d.clientWidth + 0.5) what.push(`${el.tagName.toLowerCase()}.${[...el.classList].join('.')}`)
    }
    return { by: d.scrollWidth - d.clientWidth, what: what.slice(0, 5) }
  })
}

async function fits(page: Page, at: string): Promise<void> {
  await expect(page.getByRole('heading', { level: 1 }), at).toBeVisible()
  await expect(page.getByRole('main').locator('.skeleton'), at).toHaveCount(0)
  const o = await overflow(page)
  expect(o.by, `${at} is wider than the window by ${o.by} px: ${o.what.join(', ')}`).toBeLessThanOrEqual(0)
}

for (const [scenario, paths] of Object.entries(pages)) {
  test.describe(`the ${scenario} scenario`, () => {
    test.use({ scenario })

    for (const width of widths) {
      test(`every page fits ${width} px`, async ({ page, pco }) => {
        await page.setViewportSize({ width, height: 812 })
        await signIn(page, pco, paths[0] ?? '/')
        for (const path of paths) {
          await page.goto(path)
          await fits(page, `${path} at ${width} px`)
        }
      })
    }
  })
}

// The top bar holds its pills at 320 px however long the words of the worst
// one are.
async function topBarFits(page: Page, what: string): Promise<void> {
  const bar = await page.locator('.topbar').evaluate((el) => ({ scroll: el.scrollWidth, client: el.clientWidth }))
  expect(bar.scroll, `the top bar ${what}`).toBeLessThanOrEqual(bar.client)
  await fits(page, what)
}

async function manyProblems(fake: FakeDaemon, complete: boolean): Promise<void> {
  const st = populated()
  st.problems = Array.from({ length: 12 }, (_, i) => `problem ${i + 1} of the test`)
  st.complete = complete
  await fake.post('/state', st)
}

test('the top bar fits 320 px with many problems, and whatever its worst pill says', async ({ page, pco, fake, watch }) => {
  // The browser's lines for the page's reads that pco web answers with 502.
  watch.allow(/^console error on \S+: Failed to load resource: the server responded with a status of 502\b/)
  await page.setViewportSize({ width: 320, height: 700 })
  await signIn(page, pco, '/routes')
  await manyProblems(fake, true)
  await expect(page.getByRole('link', { name: '12 problems' })).toBeVisible()
  await topBarFits(page, 'with 12 problems')

  await manyProblems(fake, false)
  await expect(page.getByRole('group', { name: 'Status of pco' })).toContainText('Inventoryincomplete')
  await topBarFits(page, 'with the inventory incomplete')

  await fake.kill()
  await expect(page.getByRole('group', { name: 'Status of pco' })).toContainText('no answer')
  await topBarFits(page, 'with the daemon gone')
})
