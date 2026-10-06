import { readdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { admin, expect, populated, test, withTicket } from './fixtures'

// What the policy of pco web allows the page, and text from guests. The
// fixtures fail every test on a violation of the policy; here the page is
// made to load all of its code, the lazy parts too, and the licences.

// The build pco web serves: make ui-e2e builds bin/pco from it.
const assets = fileURLToPath(new URL('../../internal/web/ui/dist/assets/', import.meta.url))

test('the page, each part it loads later and the licences keep to the policy', async ({ page, context, pco, watch, browserName }) => {
  if (browserName === 'webkit') {
    // WebKit shows a text file in a document of its own making, whose <pre>
    // has a style attribute; the policy refuses it, and the licences show
    // without their lines wrapped. Nothing of pco's runs there.
    watch.allow(/^policy violation on https:\/\/[^/]+\/licenses\.txt: style-src-attr refused inline at https:\/\/[^/]+\/licenses\.txt:1$/)
    watch.allow(/^console error on https:\/\/[^/]+\/licenses\.txt: Refused to apply a stylesheet because its hash, its nonce, or 'unsafe-inline' does not appear/)
  }
  const loaded = new Set<string>()
  page.on('request', (r) => loaded.add(new URL(r.url()).pathname))
  await withTicket(context, pco, admin)
  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible()

  const later = readdirSync(assets)
    .filter((f) => f.endsWith('.js'))
    .map((f) => `/assets/${f}`)
    .filter((path) => !loaded.has(path))
  for (const path of later) {
    await page.evaluate(async (src) => {
      await import(src)
    }, path)
  }

  const res = await page.goto('/licenses.txt')
  expect(res?.status()).toBe(200)
  await expect(page.getByText(/react/i).first()).toBeVisible()
})

test('a guest name is shown as text, its controls as markers', async ({ page, context, pco, fake }) => {
  const name = 'evil‮txt.exe<img src=x onerror="window.pcoRan=1">'
  await withTicket(context, pco, admin)
  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible()

  const st = populated()
  st.unapproved = [{ kind: 'qemu', vmid: 166, name, identity: 'uuid:166', hostnames: ['evil.example.com'], why: ['admission mode approve'] }]
  await fake.post('/state', st)

  const banner = page.locator('[data-banner="approval"]')
  await expect(banner).toContainText('evil⟨U+202E⟩txt.exe<img src=x onerror="window.pcoRan=1">')
  await expect(banner.getByText('⟨U+202E⟩', { exact: true })).toBeVisible()
  await expect(page.locator('img')).toHaveCount(0)
  expect(await page.evaluate(() => 'pcoRan' in window)).toBe(false)
})
