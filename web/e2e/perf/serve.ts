import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { test as base } from '@playwright/test'

import { freePort } from '../fixtures'

// The page of the performance test, built for production (vite.config.ts
// here) and served by Vite, once for each worker that runs a test of this
// directory: the other suites never build it.
const web = fileURLToPath(new URL('../..', import.meta.url))
const vite = 'node_modules/.bin/vite'
const config = 'e2e/perf/vite.config.ts'

function build(): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn(vite, ['build', '--config', config, '--logLevel', 'warn'], { cwd: web, stdio: 'inherit' })
    child.once('error', reject)
    child.once('exit', (code) => (code === 0 ? resolve() : reject(new Error(`vite build of the performance page ended with ${code}`))))
  })
}

async function answers(url: string, within: number): Promise<void> {
  const until = Date.now() + within
  for (;;) {
    try {
      if ((await fetch(url)).ok) return
    } catch {
      // not yet
    }
    if (Date.now() > until) throw new Error(`the performance page does not answer at ${url}`)
    await new Promise((resolve) => setTimeout(resolve, 100))
  }
}

export const test = base.extend<object, { perfPage: string }>({
  perfPage: [
    // eslint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      await build()
      const port = await freePort()
      const server = spawn(vite, ['preview', '--config', config, '--host', '127.0.0.1', '--port', String(port), '--strictPort'], { cwd: web, stdio: 'ignore' })
      try {
        const url = `http://127.0.0.1:${port}`
        await answers(url, 30_000)
        await provide(url)
      } finally {
        server.kill()
      }
    },
    { scope: 'worker', timeout: 120_000 },
  ],
  baseURL: async ({ perfPage }, provide) => provide(perfPage),
})

export { expect } from '@playwright/test'
