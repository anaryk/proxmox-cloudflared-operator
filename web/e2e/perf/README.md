# Flow map performance

`perf.spec.ts` measures the flow map of the Overview against a baseline drawn
with nothing but React and the DOM, both in the same run, and holds the map to
the budgets below. Until `src/flow/FlowMap.tsx` exists, only the
baseline runs and the map's test is skipped.

## Running it

From `web/`, with the packages installed (`make ui-test` installs them):

```sh
node_modules/.bin/playwright install chromium   # once
node_modules/.bin/playwright test e2e/perf
```

Playwright builds the page (`vite.config.ts` here, into
`test-results/perf-page/`) and serves it on port 4174. Each line of output
gives one renderer's figures; the samples are attached to the test's results
in `test-results/playwright/`. To look at the page, serve it with
`node_modules/.bin/vite preview --config e2e/perf/vite.config.ts` and open
`/?renderer=baseline` or `/?renderer=map`.

Shared runners are noisy, and a single run says little: compare runs, not a
run against a number from another machine.

## The scene

`harness.ts` builds the map at its render budget: 150 cards
(46 zone cards with 240 hostname rows, 12 edge nodes, 12 connectors, 20 paths,
60 target cards with 240 access points) and 400 edges. The trunk of the first
tunnel has four lanes, and it and 100 port edges carry dots, from 1.6 to 14 a
second through `dotsPerSecond`, which turns the rate of an edge into dots. One
loop moves every dot, at most 400 at once. The window is 1440 by 900; the map
fits its width and is given the height of the whole graph, so every card, edge
and dot is in its view while the window shows the top of it.

A state change moves 20 routes on to their next state: their rows, their
access points and their port edges change, every other object of the model
stays the same.

## What is measured

- **State-to-paint**: from applying a change to the end of the first frame
  painted after the map's last change for it (the commit and whatever the
  renderer does in the frames after; the dots do not count as a change). The
  95th percentile over 40 changes, after 5 that are not counted.
- **Frame time**: the intervals between the timestamps of `requestAnimationFrame`
  while the dots run for 10 s, the 95th percentile over every frame of the
  run, and the tasks over 50 ms (`PerformanceObserver`, `longtask`). The output
  also gives the rendering work of a frame, for reading the figures; no budget
  applies to it.
- **Size**: the gzip of the map's lazy chunk, by `make ui-budget`
  (`scripts/size-budget.mjs`), not here.

## Budgets

| Figure | The map passes when |
|---|---|
| state-to-paint p95 | at most 1.5 × the baseline's + 30 ms |
| frame time p95 | at most the baseline's + 4 ms |
| long tasks | no more than the baseline's |
| map chunk | at most 90 KB gzip |

The 30 ms is the tolerance for a shared runner's noise, not a margin to spend.
The CI runner is the reference; a laptop's figures are reported and decide
nothing.

## A renderer on this page

The page mounts `src/flow/FlowMap.tsx` (its default export, a `FlowMap` of
`src/flow/types.ts`) as it mounts the baseline. The map has to attach the
group its dots are drawn into and register its moving edges with the motion
it is given; the page waits for the first dot before it measures.
