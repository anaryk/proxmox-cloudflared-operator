import { existsSync } from 'node:fs'

import type { Page, TestInfo } from '@playwright/test'

import type { FrameRun } from './harness'
import { expect, test } from './serve'

// The flow map against the baseline, both measured in this run, within the
// budgets README.md lists. The baseline always runs; the map once
// src/flow/FlowMap.tsx exists. README.md says how to run it.

const changes = 40 // the p95 over 20 is nearly the maximum
const warmup = 5
const run = 10_000 // ms of dots

// Budgets relative to the baseline. The 30 ms is the tolerance for a shared
// runner's noise, not a margin to spend.
const paintFactor = 1.5
const paintNoise = 30
const frameSlack = 4

const mapExists = () => existsSync(new URL('../../src/flow/FlowMap.tsx', import.meta.url))

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

async function measure(page: Page, renderer: string, info: TestInfo): Promise<Result> {
  await page.goto(`/?renderer=${renderer}`)
  await page.waitForFunction(() => window.perf?.ready === true || window.perf?.error !== undefined)
  expect(await page.evaluate(() => window.perf?.error), `the page of ${renderer}`).toBeUndefined()

  const paints = await page.evaluate(([count, before]) => {
    if (!window.perf) throw new Error('no harness on the page')
    return window.perf.stateToPaint(count, before)
  }, [changes, warmup] as const)
  const frames: FrameRun = await page.evaluate((ms) => {
    if (!window.perf) throw new Error('no harness on the page')
    return window.perf.frames(ms)
  }, run)

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
  await info.attach(renderer, { body: JSON.stringify({ ...result, paints, frames }), contentType: 'application/json' })
  console.log(
    `${renderer.padEnd(9)} state-to-paint p95 ${result.paint.toFixed(1)} ms, frame p95 ${result.frame.toFixed(1)} ms` +
      ` (work ${result.work.toFixed(1)} ms) over ${result.frames} frames, ${result.longTasks} long tasks, ${result.dots} dots`,
  )
  return result
}

test('baseline', async ({ page }, info) => {
  await measure(page, 'baseline', info)
})

test.describe('map', () => {
  test.skip(!mapExists(), 'no map component yet')

  test('stays within the budgets of the baseline', async ({ page }, info) => {
    const base = await measure(page, 'baseline', info)
    const map = await measure(page, 'map', info)
    expect.soft(map.paint, 'state-to-paint p95, ms').toBeLessThanOrEqual(paintFactor * base.paint + paintNoise)
    expect.soft(map.frame, 'frame p95, ms').toBeLessThanOrEqual(base.frame + frameSlack)
    expect.soft(map.longTasks, 'long tasks').toBeLessThanOrEqual(base.longTasks)
  })
})
