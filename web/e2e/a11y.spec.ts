import AxeBuilder from '@axe-core/playwright'
import type { Page } from '@playwright/test'

import { expect, populated, reader, signIn, test } from './fixtures'

// Accessibility: axe finds nothing against WCAG 2.2 AA on any page, in the
// colour scheme of the project (Chromium runs both); the flow map is one
// stop of the Tab key, which the link before it skips; and what takes the
// focus is never hidden under the top bar or the events strip.

const tags = ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa']

// audit runs axe over the page laid out whole, in a window as tall as the
// page: in a shorter one the events strip, which stays at the foot of the
// window, lies over whatever is scrolled under it, and axe takes a control
// there for one too small to hit.
async function audit(page: Page, what: string): Promise<void> {
  await expect(page.getByRole('main').locator('.skeleton'), what).toHaveCount(0)
  const size = page.viewportSize() ?? { width: 1440, height: 900 }
  for (let i = 0; i < 3; i++) {
    const height = await page.evaluate(() => document.documentElement.scrollHeight)
    if (height <= (page.viewportSize()?.height ?? 0)) break
    await page.setViewportSize({ width: size.width, height: height + 40 })
  }
  const result = await new AxeBuilder({ page }).withTags(tags).analyze()
  await page.setViewportSize(size)
  const found = result.violations.map((v) => `${v.id} (${v.impact ?? ''}): ${v.help}, at ${v.nodes.map((n) => n.target.join(' ')).slice(0, 3).join('; ')}`)
  expect(found, `what axe finds on ${what}`).toEqual([])
}

async function auditAll(page: Page, paths: readonly string[]): Promise<void> {
  for (const path of paths) {
    await page.goto(path)
    await expect(page.getByRole('heading', { level: 1 }), path).toBeVisible()
    await audit(page, path)
  }
}

test.describe('every page of the populated scenario', () => {
  const groups: Record<string, string[]> = {
    'the overview and the routes': ['/', '/?list=1', '/routes', '/routes/www.example.com?owner=qemu%2F101', '/routes/plan', '/routes/manual/new', '/routes/manual/status'],
    'the guests and the networks': ['/guests', '/guests/qemu/101', '/guests/qemu/103', '/guests/claims', '/networks'],
    'the edge': ['/edge/credentials', '/edge/credentials/cred1', '/edge/zones', '/edge/zones/example.com', '/edge/tunnels', '/edge/tunnels/acc1'],
    'the events, the doctor and the settings': ['/events', '/doctor', '/settings', '/setup'],
  }
  for (const [name, paths] of Object.entries(groups)) {
    test(name, async ({ page, pco }) => {
      await signIn(page, pco, paths[0] ?? '/')
      await auditAll(page, paths)
    })
  }
})

test('the sign-in page, and the session ended over a page', async ({ page, context, pco, fake }) => {
  await page.goto('/routes')
  await expect(page.getByRole('heading', { level: 1, name: 'Sign in to pco' })).toBeVisible()
  await audit(page, 'the sign-in page')

  await signIn(page, pco, '/routes')
  await context.clearCookies({ name: 'PVEAuthCookie' })
  const st = populated()
  st.problems = [...st.problems, 'a problem of the new state']
  await fake.post('/state', st)
  await expect(page.getByRole('dialog', { name: 'Your session has ended' })).toBeVisible()
  await audit(page, 'the dialog of an ended session')
})

test('the dialogs, the drawer and the menus', async ({ page, pco, viewport }) => {
  await signIn(page, pco, '/routes/plan')
  await page.getByRole('button', { name: 'Review and confirm' }).click()
  await expect(page.getByRole('dialog', { name: 'Confirm what waits' })).toBeVisible()
  await audit(page, 'the confirmation')
  await page.keyboard.press('Escape')

  await page.goto('/guests')
  await page.getByRole('table', { name: 'Guests waiting for approval' }).getByRole('button', { name: 'Approve' }).first().click()
  await expect(page.getByRole('dialog', { name: 'Approve a guest' })).toBeVisible()
  await audit(page, 'the approval')
  await page.keyboard.press('Escape')

  await page.getByRole('button', { name: /user menu$/ }).click()
  await expect(page.locator('.menu-who')).toBeVisible()
  await audit(page, 'the user menu')
  await page.getByRole('button', { name: 'About pco' }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await audit(page, 'the About dialog')
  await page.keyboard.press('Escape')

  if ((viewport?.width ?? 0) >= 720) {
    await page.goto('/')
    await page.getByRole('group', { name: 'Flow map' }).getByRole('button', { name: /^www\.example\.com, active/ }).click()
    await expect(page.getByRole('complementary')).toBeVisible()
    await audit(page, 'the drawer of the map')
  } else {
    await page.goto('/')
    await page.getByRole('button', { name: 'Open navigation' }).click()
    await expect(page.getByRole('navigation').first()).toBeVisible()
    await audit(page, 'the navigation of a phone')
  }
})

test.describe('the other scenarios', () => {
  for (const [scenario, paths] of Object.entries({
    'first-run': ['/', '/setup'],
    rogue: ['/', '/edge/tunnels/6ab3311bfe7c52d93098e661558a14c2', '/edge/tunnels/b111e10396903a33c8edf38dbcd9eb8f'],
    frozen: ['/edge/zones', '/edge/zones/example.shop', '/routes/plan'],
    'egress-off': ['/'],
    hold: ['/', '/routes', '/setup'],
    'tagged-empty': ['/', '/routes'],
    outage: ['/'],
  })) {
    test.describe(scenario, () => {
      test.use({ scenario })
      test(`its pages`, async ({ page, pco }) => {
        await signIn(page, pco, paths[0] ?? '/')
        await auditAll(page, paths)
      })
    })
  }

  test.describe('reader', () => {
    test.use({ scenario: 'reader' })
    test('what a reader sees', async ({ page, pco }) => {
      await signIn(page, pco, '/', reader)
      await auditAll(page, ['/', '/routes', '/guests', '/settings', '/setup'])
      await page.goto('/doctor')
      await page.getByRole('button', { name: 'Run the checks' }).click()
      await expect(page.getByRole('region', { name: 'Result' })).toBeVisible()
      await audit(page, 'the doctor of a reader')
    })
  })
})

test.describe('the keyboard and the flow map', () => {
  test.skip(({ viewport }) => (viewport?.width ?? 0) < 720, 'below 720 px the Overview shows the chain list instead of the map')

  const inMap = (page: Page) => page.evaluate(() => Boolean(document.activeElement?.closest('.fm-frame')))

  test('the map is one stop of the Tab key, and its arrows move along the chains', async ({ page, pco, browserName }) => {
    // Safari moves the focus to links only with the Option key held.
    const tab = browserName === 'webkit' ? 'Alt+Tab' : 'Tab'
    await signIn(page, pco)
    const map = page.getByRole('group', { name: 'Flow map' })
    await expect(map.locator('[tabindex="0"]')).toHaveCount(1)
    await expect(map.locator('[data-item]').nth(10)).toBeAttached()

    await page.getByRole('link', { name: 'Skip the map' }).focus()
    await page.keyboard.press(tab)
    expect(await inMap(page), 'the Tab after the skip link enters the map').toBe(true)
    const first = await page.evaluate(() => (document.activeElement as HTMLElement).dataset.item)

    await page.keyboard.press('ArrowRight')
    expect(await inMap(page)).toBe(true)
    const next = await page.evaluate(() => (document.activeElement as HTMLElement).dataset.item)
    expect(next, 'the right arrow follows a line').not.toBe(first)
    await expect(map.locator('[tabindex="0"]')).toHaveCount(1)
    await page.keyboard.press('Home')
    await page.keyboard.press('End')
    expect(await inMap(page)).toBe(true)

    await page.keyboard.press(tab)
    expect(await inMap(page), 'one Tab leaves the map').toBe(false)
  })

  test('the skip link moves the focus past the map', async ({ page, pco }) => {
    await signIn(page, pco)
    const skip = page.getByRole('link', { name: 'Skip the map' })
    await skip.focus()
    await expect(skip).toBeVisible()
    await page.keyboard.press('Enter')
    await expect(page.locator('#flow-end')).toBeFocused()
    expect(await inMap(page)).toBe(false)
  })
})

// Every control is a target of at least 24 by 24 px, as the accessibility
// of the interface asks: not only where nothing else is near.
test('the controls of the pages are targets of at least 24 by 24 px', async ({ page, pco }) => {
  await signIn(page, pco, '/guests')
  const small: string[] = []
  for (const path of ['/guests', '/guests/qemu/101', '/guests/claims', '/routes', '/routes/manual/status', '/routes/plan', '/edge/credentials', '/edge/zones', '/edge/tunnels', '/events', '/settings']) {
    await page.goto(path)
    await expect(page.getByRole('heading', { level: 1 }), path).toBeVisible()
    await expect(page.getByRole('main').locator('.skeleton'), path).toHaveCount(0)
    const found = await page.evaluate(() => {
      const targets = document.querySelectorAll<HTMLElement>(
        'main button, main select, main input[type="checkbox"], main input[type="radio"], main .table td a, main summary',
      )
      const out: string[] = []
      for (const el of targets) {
        const r = el.getBoundingClientRect()
        if (r.width === 0 && r.height === 0) continue
        if (r.width < 23.5 || r.height < 23.5) out.push(`${el.tagName.toLowerCase()} "${(el.textContent || el.getAttribute('aria-label') || '').trim().slice(0, 30)}" ${r.width.toFixed(1)}x${r.height.toFixed(1)}`)
      }
      return out
    })
    small.push(...found.map((f) => `${path}: ${f}`))
  }
  expect(small, 'controls smaller than 24 by 24 px').toEqual([])
})

for (const strip of ['closed', 'open']) {
  test(`what takes the focus is not hidden under the top bar or the events strip, ${strip}`, async ({ page, pco, browserName }) => {
    // Safari moves the focus to links only with the Option key held.
    const tab = browserName === 'webkit' ? 'Alt+Tab' : 'Tab'
    await page.setViewportSize({ width: 1280, height: 600 })
    await signIn(page, pco, '/guests')
    if (strip === 'open') {
      await page.locator('.strip > summary').click()
      await expect(page.getByRole('link', { name: 'All events' })).toBeVisible()
    }
    await page.locator('body').focus()
    const hidden: string[] = []
    // Forwards down the page, which the strip covers at its foot, and back up
    // it, which the top bar covers at its head.
    for (const [key, stops] of [
      [tab, 120],
      [`Shift+${tab}`, 60],
    ] as const) {
      for (let i = 0; i < stops; i++) {
        await page.keyboard.press(key)
        const where = await page.evaluate(() => {
          const el = document.activeElement
          if (!el || el === document.body || el.closest('.topbar, .strip, .nav, .skiplink, dialog')) return undefined
          const r = el.getBoundingClientRect()
          const bar = document.querySelector('.topbar')?.getBoundingClientRect().bottom ?? 0
          const foot = document.querySelector('.strip')?.getBoundingClientRect().top ?? window.innerHeight
          return r.top < bar - 1 || r.bottom > foot + 1 ? `${el.tagName.toLowerCase()} "${el.textContent?.slice(0, 30) ?? ''}" at ${r.top.toFixed(0)}-${r.bottom.toFixed(0)}` : undefined
        })
        if (where) hidden.push(`${key}: ${where}`)
      }
    }
    expect(hidden, 'focused controls under the top bar or the strip').toEqual([])
  })
}
