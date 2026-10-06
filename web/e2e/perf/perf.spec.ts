import type { Page, TestInfo } from '@playwright/test'

import type { FrameRun } from './harness'
import { expect, test } from './serve'

// The flow map against the baseline, both measured in this run on each
// scene, within the budgets README.md lists. README.md says how to run it.

const changes = 40 // the p95 over 20 is nearly the maximum
const warmup = 5
const run = 10_000 // ms of dots

// Budgets relative to the baseline. The 30 ms is the tolerance for a shared
// runner's noise, not a margin to spend.
const paintFactor = 1.5
const paintNoise = 30
const frameSlack = 4

// The graph at the render budget, and the fake daemon's scenarios of many
// routes as the Overview folds them.
const scenes = ['budget', 'large', 'outage', 'wide']

function p95(values: readonly number[]): number {
  const sorted = [...values].sort((a, b) => a - b)
  return sorted[Math.max(0, Math.ceil(0.95 * sorted.length) - 1)] ?? Number.NaN
}

interface Result {
  renderer: string
  paint: number // state-to-paint p95, ms
  frame: number // frame interval p95, ms
  work: number // rendering work per frame p95, ms
  longTasks: number
  frames: number
  dots: number
}

async function measure(page: Page, scene: string, renderer: string, info: TestInfo): Promise<Result> {
  await page.goto(`/?scene=${scene}&renderer=${renderer}`)
  await page.waitForFunction(() => window.perf?.ready === true || window.perf?.error !== undefined, undefined, { timeout: 60_000 })
  expect(await page.evaluate(() => window.perf?.error), `the page of ${renderer}`).toBeUndefined()

  const paints = await page.evaluate(([count, before]) => {
    if (!window.perf) throw new Error('no harness on the page')
    return window.perf.stateToPaint(count, before)
  }, [changes, warmup] as const)
  const frames: FrameRun = await page.evaluate((ms) => {
    if (!window.perf) throw new Error('no harness on the page')
    return window.perf.frames(ms)
  }, run)
  const scale = await page.evaluate(() => ({ pipeline: window.perf?.pipeline ?? [], cards: window.perf?.cards ?? 0, edges: window.perf?.edges ?? 0 }))

  expect(paints).toHaveLength(changes)
  expect(frames.dots, 'dots on the map').toBeGreaterThan(0)
  const result = {
    renderer,
    paint: p95(paints),
    frame: p95(frames.intervals),
    work: p95(frames.work),
    longTasks: frames.longTasks.length,
    frames: frames.intervals.length,
    dots: Math.round(frames.dots),
  }
  await info.attach(`${scene} ${renderer}`, { body: JSON.stringify({ ...result, paints, frames, pipeline: scale.pipeline }), contentType: 'application/json' })
  const pipeline = scale.pipeline.length > 0 ? `, model and layout p95 ${p95(scale.pipeline).toFixed(1)} ms` : ''
  console.log(
    `${scene.padEnd(7)} ${renderer.padEnd(9)} ${scale.cards} cards, ${scale.edges} edges: state-to-paint p95 ${result.paint.toFixed(1)} ms${pipeline}, frame p95 ${result.frame.toFixed(1)} ms` +
      ` (work ${result.work.toFixed(1)} ms) over ${result.frames} frames, ${result.longTasks} long tasks, ${result.dots} dots`,
  )
  return result
}

for (const scene of scenes) {
  test(`${scene}: the map stays within the budgets of the baseline`, async ({ page }, info) => {
    const base = await measure(page, scene, 'baseline', info)
    const map = await measure(page, scene, 'map', info)
    expect.soft(map.paint, 'state-to-paint p95, ms').toBeLessThanOrEqual(paintFactor * base.paint + paintNoise)
    expect.soft(map.frame, 'frame p95, ms').toBeLessThanOrEqual(base.frame + frameSlack)
    expect.soft(map.longTasks, 'long tasks').toBeLessThanOrEqual(base.longTasks)
  })
}
