import type { Page } from '@playwright/test'

import { expect, type FakeDaemon, type Pco, populated, signIn, test } from './fixtures'

// The flow map of the Overview against the populated scenario: what it
// draws, what the pointer lights, and its dots, which move only where a
// figure was measured, as fast as the figure says, and never while paused,
// under reduced motion or on figures that are stale.

test.skip(({ viewport }) => (viewport?.width ?? 0) < 720, 'below 720 px the Overview shows the chain list instead of the map')

const map = (page: Page) => page.getByRole('group', { name: 'Flow map' })
const item = (page: Page, name: string | RegExp) => map(page).getByRole('button', { name })

// The tunnel of account acc1, its connector on pve1, and the route of
// www.example.com on qemu/101, port 8080, in the populated scenario.
const tunnel = '00000000-0000-4000-8000-000000000001'
const www = { hostname: 'www.example.com', owner: 'qemu/101', target: '10.0.0.11:8080' }
const trunk = '.fm-edge-trunk'
const trunkFigure = `${trunk} .fm-figure:not(.fm-errors)`
const portLine = `[data-edge$="|${www.target}"]`

// Every route of the scenario, and every hostname of a guest that waits for
// approval, has a chain on the list. It has a row on the card of its zone too,
// unless it is a route in no zone.
const scenario = populated()
const waiting = scenario.unapproved.flatMap((g) => g.hostnames)
const chainCount = scenario.routes.length + waiting.length
const rowsOn = (zone: string) =>
  [...scenario.routes.filter((r) => r.state !== 'no-zone').map((r) => r.hostname), ...waiting].filter((hostname) => hostname.endsWith(`.${zone}`)).length

// dotsOn counts the dots that set off on the line of selector in seconds of
// the map's motion. The motion moves by the time between two frames, at most
// a tenth of a second, so a busy machine slows it down: the time is counted
// the same way. A dot is told by where it is drawn first, at the start of
// its line.
function dotsOn(page: Page, selector: string, seconds = 2): Promise<number> {
  return page.evaluate(
    async ({ selector, seconds }) => {
      const line = document.querySelector(`.fm-lines ${selector}`)
      const stroke = line?.querySelector<SVGGeometryElement>('.fm-pipe, .fm-hit')
      const layer = document.querySelector('.fm-dots g')
      if (!stroke || !layer) throw new Error(`no line ${selector} on the map, or no layer of dots`)
      let n = 0
      const count = (el: Element) => {
        const at = /translate\(([-\d.]+) ([-\d.]+)\)/.exec(el.getAttribute('transform') ?? '')
        if (at && stroke.isPointInStroke(new DOMPoint(Number(at[1]), Number(at[2])))) n++
      }
      const read = (records: MutationRecord[]) => {
        for (const r of records) {
          if (r.type === 'childList') r.addedNodes.forEach((el) => count(el as Element))
          else if (r.oldValue === 'hidden' && !(r.target as Element).hasAttribute('visibility')) count(r.target as Element)
        }
      }
      const watch = new MutationObserver(read)
      watch.observe(layer, { childList: true, attributes: true, attributeFilter: ['visibility'], attributeOldValue: true, subtree: true })
      await new Promise<void>((resolve) => {
        const began = performance.now()
        let moved = 0
        let last: number | undefined
        const frame = (now: number) => {
          if (last !== undefined) moved += Math.min(0.1, (now - last) / 1000)
          last = now
          if (moved >= seconds || now - began > seconds * 5000) resolve()
          else requestAnimationFrame(frame)
        }
        requestAnimationFrame(frame)
      })
      read(watch.takeRecords())
      watch.disconnect()
      return n
    },
    { selector, seconds },
  )
}

async function setTraffic(fake: FakeDaemon, rps: number, flows: number): Promise<void> {
  await fake.post('/traffic', { tunnels: [{ tunnelId: tunnel, rps, errorsPerSec: 0 }], routes: [{ ...www, flowsPerSec: flows }] })
}

// focusOnWww opens the map with the chains of www.example.com alone, all of
// them in view.
async function focusOnWww(page: Page, pco: Pco): Promise<void> {
  await signIn(page, pco, `/?focus=hostname:${www.hostname}`)
  await expect(item(page, /^www\.example\.com, active/)).toBeVisible()
}

test('the map draws the populated scenario, a style for each state', async ({ page, pco }) => {
  await signIn(page, pco)
  await expect(item(page, `Zone example.com, served, account Main, ${rowsOn('example.com')} hostnames`)).toBeVisible()
  await expect(item(page, 'www.example.com, active, served by qemu/101 web-1 on port 8080')).toBeVisible()
  await expect(item(page, /^www\.example\.com, conflict: hostname is held by qemu\/101/)).toBeVisible()
  await expect(item(page, /^Edge of tunnel Main · 00000000, verified: yes/)).toBeVisible()
  await expect(item(page, /^Connector on pve1 for tunnel Main · 00000000: active, ready, 4 connections/)).toBeVisible()
  await expect(item(page, /^Connector not run by pco: 198\.51\.100\.7/)).toBeVisible()
  await expect(item(page, 'Path vmbr0 · VLAN 20 · direct')).toBeVisible()
  // the guest behind the leg of an attached network: a path of its own, and the service address the connector dials
  await expect(item(page, /^Path vmbr2 · attach/)).toBeVisible()
  await expect(item(page, /^Guest zabbix qemu\/107, ports :8080 http active/)).toBeVisible()
  await expect(item(page, /^Guest web-1 qemu\/101, ports :3000 http active, 3\.7 new connections per second; :8080 http active/)).toBeVisible()

  const lines = map(page).locator('.fm-lines')
  await expect(lines.locator(trunk)).toHaveCount(1)
  await expect(lines.locator(`${trunk} .fm-lane`)).toHaveCount(4)
  await expect(lines.locator('.fm-edge-unreachable .fm-line').first()).toHaveAttribute('stroke-dasharray', '6 4')
  await expect(lines.locator('.fm-edge-withdrawn .fm-line').first()).toHaveAttribute('stroke-dasharray', '2 3')
  await expect(lines.locator('.fm-edge-rogue .fm-line')).toHaveAttribute('stroke-dasharray', '4 3')
  await expect(lines.locator(trunkFigure)).toHaveText('39.7 req/s')
  await expect(page.getByRole('list', { name: 'Legend' })).toContainText('Only lines with a measured figure move')
})

test('the pointer on a hostname lights its chain and dims the rest', async ({ page, pco }) => {
  await signIn(page, pco)
  const row = item(page, 'www.example.com, active, served by qemu/101 web-1 on port 8080')
  await row.hover()
  await expect(map(page)).toHaveClass(/fm-lighting/)
  await expect(row).not.toHaveClass(/fm-dim/)
  await expect(item(page, /^monitor\.example\.com/)).toHaveClass(/fm-dim/)

  const card = (name: string | RegExp) => item(page, name).first().locator('xpath=ancestor-or-self::*[contains(concat(" ", @class, " "), " fm-card ")][1]')
  for (const lit of [/^Edge of tunnel Main/, /^Connector on pve1/, 'Path vmbr0 · VLAN 20 · direct', /^Guest web-1 qemu\/101/]) {
    await expect(card(lit), String(lit)).not.toHaveClass(/fm-dim/)
  }
  for (const dim of [/^Guest grafana lxc\/205/, /^Guest monitor qemu\/105/, /^Edge of tunnel acc4/]) {
    await expect(card(dim), String(dim)).toHaveClass(/fm-dim/)
  }
  const lines = map(page).locator('.fm-lines')
  await expect(lines.locator(trunk)).not.toHaveClass(/fm-dim/)
  await expect(lines.locator(portLine)).not.toHaveClass(/fm-dim/)
  await expect(lines.locator('[data-edge$="|10.0.0.20:3000"]')).toHaveClass(/fm-dim/)

  await page.getByRole('heading', { level: 1 }).hover()
  await expect(map(page)).not.toHaveClass(/fm-lighting/)
})

test('the dots of the trunk follow the requests, those of a port line its connections', async ({ page, pco, fake }) => {
  await focusOnWww(page, pco)
  const lines = map(page).locator('.fm-lines')

  await setTraffic(fake, 1, 0.2)
  await expect(lines.locator(trunkFigure)).toHaveText('1.0 req/s')
  await expect(item(page, /^Guest web-1 qemu\/101, ports :8080 http active, 0\.2 new connections per second/)).toBeVisible()
  const slow = { trunk: await dotsOn(page, trunk), port: await dotsOn(page, portLine) }

  await setTraffic(fake, 500, 300)
  await expect(lines.locator(trunkFigure)).toHaveText('500.0 req/s')
  await expect(item(page, /^Guest web-1 qemu\/101, ports :8080 http active, 300\.0 new connections per second/)).toBeVisible()
  const fast = { trunk: await dotsOn(page, trunk), port: await dotsOn(page, portLine) }

  // 1 req/s is 2.7 dots a second and 500 req/s 12.3; 0.2 connections a
  // second 1.8 and 300 of them 11.4.
  expect(slow.trunk, 'dots on the trunk at 1 req/s').toBeGreaterThan(0)
  expect(slow.port, 'dots on the port line at 0.2 connections a second').toBeGreaterThan(0)
  expect(fast.trunk, `dots on the trunk at 500 req/s, against ${slow.trunk} at 1`).toBeGreaterThan(2 * slow.trunk)
  expect(fast.port, `dots on the port line at 300 connections a second, against ${slow.port} at 0.2`).toBeGreaterThan(2 * slow.port)
})

test('nothing moves while the motion is paused', async ({ page, pco, fake }) => {
  await focusOnWww(page, pco)
  await setTraffic(fake, 500, 300)
  await expect(map(page).locator(`.fm-lines ${trunkFigure}`)).toHaveText('500.0 req/s')
  expect(await dotsOn(page, trunk, 1), 'dots before the pause').toBeGreaterThan(0)

  await page.getByRole('button', { name: 'Pause motion' }).click()
  await expect(page.getByRole('button', { name: 'Resume motion' })).toHaveAttribute('aria-pressed', 'true')
  expect(await dotsOn(page, trunk)).toBe(0)
  expect(await dotsOn(page, portLine)).toBe(0)
  await expect(map(page).locator('.fm-dots g > :not([visibility])')).toHaveCount(0)
  await expect(map(page).locator(`.fm-lines ${portLine} .fm-chevrons`)).toHaveCount(1)
  await expect(page.getByRole('list', { name: 'Legend' })).toContainText('Motion is paused: chevrons and the figure stand for the dots.')

  await page.getByRole('button', { name: 'Resume motion' }).click()
  expect(await dotsOn(page, trunk, 1), 'dots after the pause').toBeGreaterThan(0)
})

test.describe('with reduced motion', () => {
  test.use({ reducedMotion: 'reduce' })

  test('nothing moves, and chevrons with the figure stand for the dots', async ({ page, pco, fake }) => {
    await focusOnWww(page, pco)
    await setTraffic(fake, 500, 300)
    await expect(map(page).locator(`.fm-lines ${trunk} .fm-chevrons .fm-figure`)).toHaveText('500.0 req/s')
    expect(await dotsOn(page, trunk)).toBe(0)
    expect(await dotsOn(page, portLine)).toBe(0)
    await expect(map(page).locator(`.fm-lines ${portLine} .fm-chevrons`)).toHaveCount(1)
    await expect(page.getByRole('list', { name: 'Legend' })).toContainText('Reduced motion: chevrons and the figure stand for the dots.')

    await page.emulateMedia({ reducedMotion: 'no-preference' })
    expect(await dotsOn(page, trunk, 1), 'dots once motion is no longer reduced').toBeGreaterThan(0)
  })
})

test('figures that are stale stop the dots of their lines', async ({ page, pco, fake }) => {
  await focusOnWww(page, pco)
  await setTraffic(fake, 500, 300)
  await expect(map(page).locator(`.fm-lines ${trunkFigure}`)).toHaveText('500.0 req/s')

  await fake.post('/traffic', { tunnels: [{ tunnelId: tunnel, stale: true }], routes: [{ ...www, stale: true }] })
  await expect(map(page).locator(`.fm-lines ${trunk} .fm-since`)).toContainText('no data since')
  await expect(map(page).locator(`.fm-lines ${trunk}`)).toHaveClass(/fm-muted/)
  expect(await dotsOn(page, trunk)).toBe(0)
  expect(await dotsOn(page, portLine)).toBe(0)
})

test('nothing moves while the page has no live data', async ({ page, pco, fake, watch }) => {
  // The browser's lines for the page's reads that pco web answers with 502.
  watch.allow(/^console error on \S+: Failed to load resource: the server responded with a status of 502\b/)
  await focusOnWww(page, pco)
  await setTraffic(fake, 500, 300)
  await expect(map(page).locator(`.fm-lines ${trunkFigure}`)).toHaveText('500.0 req/s')
  expect(await dotsOn(page, trunk, 1), 'dots while live').toBeGreaterThan(0)

  await fake.kill()
  await expect(page.getByRole('group', { name: 'Status of pco' })).toContainText('no answer')
  await expect(map(page)).toHaveClass(/fm-stale/)
  expect(await dotsOn(page, trunk)).toBe(0)
  expect(await dotsOn(page, portLine)).toBe(0)
})

test.describe('with the egress filter off', () => {
  test.use({ scenario: 'egress-off' })

  test('the port lines show state only, the legend says why, and the trunk moves', async ({ page, pco }) => {
    await signIn(page, pco)
    await expect(page.getByRole('list', { name: 'Legend' })).toContainText('Port lines show state only: the egress filter is off: pco has no per-guest counters')
    await expect(item(page, 'Guest web-1 qemu/101, ports :3000 http active; :8080 http active')).toBeVisible()
    await expect(map(page).locator('.fm-lines .fm-edge-plain[data-edge*="|"] .fm-chevrons')).toHaveCount(0)
    expect(await dotsOn(page, portLine)).toBe(0)
    expect(await dotsOn(page, trunk), 'dots on the trunk').toBeGreaterThan(0)
  })
})

test('the list view gives every chain as text', async ({ page, pco }) => {
  await signIn(page, pco)
  await page.getByRole('button', { name: 'List view' }).click()
  await expect(page).toHaveURL(/[?&]list=1/)
  await expect(map(page)).toHaveCount(0)
  const chains = page.getByRole('list', { name: 'Hostnames and their chains' })
  await expect(chains.getByRole('listitem')).toHaveCount(chainCount)
  await expect(page.getByRole('list', { name: 'Traffic between the edge and the connectors' })).toContainText('39.7 req/s')

  await chains.getByRole('button', { name: 'active www.example.com web-1 qemu/101' }).click()
  await expect(page.getByRole('list', { name: 'The chain of www.example.com' }).getByRole('listitem')).toHaveText([
    /^Zone ?example\.com · served · Main$/,
    /^Edge ?Main · 00000000 · verified: yes$/,
    /^Connector ?pve1 · active, ready, 4 connections$/,
    /^Path ?vmbr0 · VLAN 20 · direct$/,
    /^Target ?qemu\/101 web-1 · 10\.0\.0\.11:8080 http · level port$/,
  ])
  await expect(chains).toContainText('3.1 new connections per second from the connector to 10.0.0.11:8080')

  await page.getByRole('button', { name: 'List view' }).click()
  await expect(map(page)).toBeVisible()
})
