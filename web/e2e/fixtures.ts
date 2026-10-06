// The harness of the browser suite: for each test a fake Proxmox VE
// (hack/fakepve), a fake daemon on a unix socket (hack/fakepco) with a
// scenario, and the real pco web between them and the browser, as
// pco-web.service runs it. make ui-e2e builds the three into bin/.
//
// Every test fails on a violation of the Content-Security-Policy, on an
// error in the console of a page, and on an answer of pco web that carries a
// header of CORS or another policy than the page's.

import { type ChildProcess, spawn } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { type BrowserContext, expect, type Locator, type Page, test as base } from '@playwright/test'

import type { State } from '../src/api/types.gen'

export { expect }

const repo = fileURLToPath(new URL('../..', import.meta.url))
const bin = (name: string) => join(repo, 'bin', name)

// The policy of every answer of pco web.
export const contentSecurityPolicy =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; " +
  "manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; " +
  "require-trusted-types-for 'script'; trusted-types 'none'"

// A user of the fake Proxmox VE: the PVEAuthCookie its page sets, and an API
// token.
export interface User {
  user: string
  ticket: string
  token: string
}

export const admin: User = {
  user: 'alice@pve',
  ticket: 'PVE:alice@pve:66F1E2D3::YWxpY2U=',
  token: 'alice@pve!pco=0b5c3e7e-1d2f-4a6b-9c8d-7e6f5a4b3c2d',
}

export const reader: User = {
  user: 'bob@pve',
  ticket: 'PVE:bob@pve:66F1E2D4::Ym9i',
  token: 'bob@pve!pco=5e0c1f7a-92b4-4d3e-8a1b-2c3d4e5f6a7b',
}

// The guests the reader may audit, of those the scenarios have.
const readerGuests = ['qemu/101', 'lxc/205']

// The actions only an admin may take, as their buttons read. A reader sees
// none of them, or sees them disabled with what they need.
export const adminAction = /^(Approve|Revoke|Resolve|Confirm|Apply|Adopt|Sync|Add|Save|Import|Delete|Remove|Restart|Rotate|Check|Start publishing|Hand to|Pin|Take back|Review and confirm|New manual route|Return to observe-only)\b/

const secretOf = (u: User) => u.token.slice(u.token.indexOf('=') + 1)

const users = {
  users: [
    {
      user: admin.user,
      privileges: { '/': ['Sys.Audit', 'Sys.Modify'] },
      guests: [],
      tickets: [admin.ticket],
      tokens: [{ id: 'pco', secret: secretOf(admin), privsep: false }],
    },
    {
      user: reader.user,
      privileges: { '/': ['Sys.Audit'] },
      guests: readerGuests,
      tickets: [reader.ticket],
      tokens: [{ id: 'pco', secret: secretOf(reader), privsep: false }],
    },
  ],
}

// The process groups of the harness that run. Each process runs in a group
// of its own, which is stopped as one; whatever is left when the worker
// ends is killed with it.
const groups = new Set<number>()
process.once('exit', () => {
  for (const pid of groups) {
    try {
      process.kill(-pid, 'SIGKILL')
    } catch {
      // gone already
    }
  }
})

function signalGroup(child: ChildProcess, signal: NodeJS.Signals): void {
  try {
    if (child.pid !== undefined) process.kill(-child.pid, signal)
  } catch {
    child.kill(signal)
  }
}

// A process of the harness. What it prints is kept for the report of a test
// that fails.
class Proc {
  output = ''
  #child?: ChildProcess
  #exited?: Promise<void>

  constructor(
    readonly name: string,
    readonly args: string[],
    readonly env: NodeJS.ProcessEnv = {},
  ) {}

  // start runs the process and waits for a line of its JSON log that ready
  // takes; ready answers undefined for the others.
  start<T>(ready: (line: Record<string, unknown>) => T | undefined, timeout = 20_000): Promise<T> {
    const child = spawn(bin(this.name), this.args, {
      env: { PATH: process.env.PATH, HOME: process.env.HOME, ...this.env },
      stdio: ['ignore', 'pipe', 'pipe'],
      detached: true,
    })
    this.#child = child
    const pid = child.pid
    if (pid !== undefined) groups.add(pid)
    this.#exited = new Promise((resolve) =>
      child.once('exit', () => {
        if (pid !== undefined) groups.delete(pid)
        resolve()
      }),
    )
    return new Promise<T>((resolve, reject) => {
      let done = false
      let pending = ''
      const timer = setTimeout(() => {
        done = true
        reject(new Error(`${this.name} did not start within ${timeout} ms:\n${this.output}`))
      }, timeout)
      const read = (chunk: Buffer) => {
        const text = chunk.toString()
        this.output += text
        if (done) return
        pending += text
        for (let nl = pending.indexOf('\n'); nl >= 0; nl = pending.indexOf('\n')) {
          const line = pending.slice(0, nl)
          pending = pending.slice(nl + 1)
          let found: T | undefined
          try {
            found = ready(JSON.parse(line) as Record<string, unknown>)
          } catch {
            continue
          }
          if (found !== undefined) {
            done = true
            clearTimeout(timer)
            resolve(found)
            return
          }
        }
      }
      child.stdout?.on('data', read)
      child.stderr?.on('data', read)
      child.once('error', (e) => {
        done = true
        clearTimeout(timer)
        reject(new Error(`${this.name}: ${e.message}; make ui-e2e builds it into bin/`))
      })
      child.once('exit', (code, signal) => {
        if (done) return
        done = true
        clearTimeout(timer)
        reject(new Error(`${this.name} ended (${code ?? signal}) before it was ready:\n${this.output}`))
      })
    })
  }

  // stop ends the process and its group with signal, and with SIGKILL when
  // it is still there after 10 s.
  async stop(signal: NodeJS.Signals = 'SIGTERM'): Promise<void> {
    const child = this.#child
    if (!child || child.exitCode !== null || child.signalCode !== null) return
    signalGroup(child, signal)
    const killer = setTimeout(() => signalGroup(child, 'SIGKILL'), 10_000)
    await this.#exited
    clearTimeout(killer)
  }
}

// freePort is a port of loopback nothing listened on a moment ago.
export function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer()
    srv.once('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address()
      srv.close(() => (addr && typeof addr === 'object' ? resolve(addr.port) : reject(new Error('no port'))))
    })
  })
}

// A call the fake daemon recorded.
export interface Call {
  method: string
  args: unknown
  at: string
  actor?: string
}

// The controls of the fake daemon (hack/fakepco).
export interface FakeDaemon {
  post(path: string, body?: unknown): Promise<void>
  calls(...methods: string[]): Promise<Call[]>
  clearCalls(): Promise<void>
  subscribers(): Promise<number>
  // kill ends the daemon at once, as a crash would.
  kill(): Promise<void>
}

// pco web of the test, at https://127.0.0.1:<port>.
export interface Pco {
  url: string
  host: string
  // stop stops pco web, as systemctl stop does; start starts it again on the
  // same address.
  stop(): Promise<void>
  start(): Promise<void>
}

// Watch is what a test lets through of what the guard reports, each a line
// "<what> on <url>: <text>". The browser's own line for an answer of 401,
// which a page without a session gets on its first call, always passes.
export class Watch {
  readonly allowed: RegExp[] = [/^console error on \S+: Failed to load resource: the server responded with a status of 401\b/]

  allow(line: RegExp): void {
    this.allowed.push(line)
  }
}

interface Options {
  scenario: string
  hsts: boolean
}

interface Fixtures {
  pco: Pco
  fake: FakeDaemon
  watch: Watch
}

interface Harness {
  pco: Pco
  fake: FakeDaemon
}

// startHarness starts the fakes and pco web in dir, each added to procs as
// it starts, so that whoever holds procs stops what started.
async function startHarness(dir: string, procs: Proc[], scenario: string, hsts: boolean): Promise<Harness> {
  writeFileSync(join(dir, 'users.json'), JSON.stringify(users))
  const pve = new Proc('fakepve', ['-listen', '127.0.0.1:0', '-users', join(dir, 'users.json'), '-cert-dir', dir, '-web-cert'])
  procs.push(pve)
  const pveURL = await pve.start((l) => (l.message === 'serving' ? String(l.url) : undefined))

  const socket = join(dir, 'pco', 'pco.sock')
  const daemon = new Proc('fakepco', ['-socket', socket, '-control', '127.0.0.1:0', '-scenario', scenario])
  procs.push(daemon)
  const control = await daemon.start((l) => (l.message === 'serving' ? String(l.control) : undefined))

  // pco web takes the address it is given, not a port of its own choosing,
  // as the hosts it answers for are that address. Another worker may take
  // the free port first: then pco web tries another.
  let host = ''
  let web: Proc | undefined
  const serving = (l: Record<string, unknown>) => (l.message === 'serving the web interface' ? true : undefined)
  for (let attempt = 1; !web; attempt++) {
    host = `127.0.0.1:${await freePort()}`
    const p = new Proc(
      'pco',
      ['--socket', socket, 'web', '--listen', host, '--cert', join(dir, 'tls.crt'), '--key', join(dir, 'tls.key'), '--pin', join(dir, 'pveproxy.crt'), '--pve-url', pveURL],
      hsts ? { PCO_WEB_HSTS: '1' } : {},
    )
    procs.push(p)
    try {
      await p.start(serving)
      web = p
    } catch (e) {
      if (attempt >= 3 || !p.output.includes('address already in use')) throw e
    }
  }
  const started = web
  const startWeb = async () => {
    await started.start(serving)
  }

  const call = async (method: string, path: string, body?: unknown): Promise<Response> => {
    const res = await fetch(control + path, {
      method,
      headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!res.ok) throw new Error(`${method} ${path} of the fake daemon: ${res.status} ${await res.text()}`)
    return res
  }
  const fake: FakeDaemon = {
    post: async (path, body) => {
      await call('POST', path, body)
    },
    calls: async (...methods) => {
      const q = new URLSearchParams(methods.map((m) => ['method', m]))
      return (await (await call('GET', `/calls?${q}`)).json()) as Call[]
    },
    clearCalls: async () => {
      await call('DELETE', '/calls')
    },
    subscribers: async () => ((await (await call('GET', '/subscribers')).json()) as { subscribers: number }).subscribers,
    kill: () => daemon.stop('SIGKILL'),
  }
  return { pco: { url: `https://${host}`, host, stop: () => web.stop(), start: startWeb }, fake }
}

// guard fails the test on what no page of pco may do: a violation of the
// policy, an error in the console or an uncaught one, and an answer with a
// header of CORS or without the exact policy.
async function guard(context: BrowserContext, origin: string, watch: Watch): Promise<() => Promise<string[]>> {
  const problems: string[] = []
  const checks: Promise<void>[] = []
  await context.exposeBinding('pcoViolation', ({ page }, text: string) => {
    problems.push(`policy violation on ${page.url()}: ${text}`)
  })
  await context.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => {
      const report = (window as unknown as { pcoViolation?: (text: string) => Promise<void> }).pcoViolation
      void report?.(`${e.effectiveDirective} refused ${e.blockedURI || 'inline'} at ${e.sourceFile || e.documentURI}:${e.lineNumber}`)
    })
  })
  context.on('console', (m) => {
    if (m.type() === 'error') problems.push(`console error on ${m.page()?.url()}: ${m.text()}`)
  })
  context.on('weberror', (e) => problems.push(`uncaught error on ${e.page()?.url()}: ${e.error().message}`))
  context.on('response', (res) => {
    if (!res.url().startsWith(origin)) return
    checks.push(
      res.allHeaders().then(
        (h) => {
          const cors = Object.keys(h).filter((k) => k.startsWith('access-control-'))
          if (cors.length > 0) problems.push(`answer on ${res.url()}: ${cors.join(', ')}`)
          if (h['content-security-policy'] !== contentSecurityPolicy) problems.push(`answer on ${res.url()}: the policy ${h['content-security-policy']}`)
        },
        () => undefined, // the page went before its headers were read
      ),
    )
  })
  return async () => {
    await Promise.race([Promise.allSettled(checks), new Promise((resolve) => setTimeout(resolve, 5000))])
    return problems.filter((p) => !watch.allowed.some((re) => re.test(p)))
  }
}

// The second argument of a fixture is named provide, not use: the linter of
// the interface takes use for React's.
export const test = base.extend<Options & Fixtures & { harness: Harness }>({
  scenario: ['populated', { option: true }],
  hsts: [false, { option: true }],
  harness: async ({ scenario, hsts }, provide, testInfo) => {
    // A unix socket needs a short path, which the test's own directory may
    // not have.
    const dir = mkdtempSync(join(tmpdir(), 'pco-e2e-'))
    const procs: Proc[] = []
    try {
      await provide(await startHarness(dir, procs, scenario, hsts))
    } finally {
      if (testInfo.status !== testInfo.expectedStatus) {
        for (const p of procs) await testInfo.attach(`${p.name}.log`, { body: p.output, contentType: 'text/plain' })
      }
      for (const p of procs.reverse()) await p.stop()
      rmSync(dir, { recursive: true, force: true })
    }
  },
  pco: async ({ harness }, provide) => provide(harness.pco),
  fake: async ({ harness }, provide) => provide(harness.fake),
  baseURL: async ({ harness }, provide) => provide(harness.pco.url),
  watch: async ({ browserName }, provide) => {
    const watch = new Watch()
    // WebKit reports as an error, now and then and under load, that it left
    // a change of size for the observers of the next frame: no error of the
    // page, and nothing it can keep WebKit from saying.
    if (browserName === 'webkit') watch.allow(/^uncaught error on \S+: ResizeObserver loop completed with undelivered notifications\.$/)
    await provide(watch)
  },
  context: async ({ context, harness, watch }, provide) => {
    const problems = await guard(context, harness.pco.url, watch)
    await provide(context)
    expect(await problems(), 'what the pages of pco did that they must not').toEqual([])
  },
})

// withTicket gives the browser the session of Proxmox VE of user, as the
// page of Proxmox VE on port 8006 of the same name sets it.
export async function withTicket(context: BrowserContext, pco: Pco, u: User): Promise<void> {
  await context.addCookies([{ name: 'PVEAuthCookie', value: u.ticket, url: pco.url, secure: true, sameSite: 'Lax' }])
}

// signIn opens path in page as user, signed in with the session of Proxmox
// VE, and waits for the page's first read of the state and its heading.
export async function signIn(page: Page, pco: Pco, path = '/', u: User = admin): Promise<void> {
  await withTicket(page.context(), pco, u)
  const read = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/v1/state' && r.ok())
  await page.goto(path)
  await read
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
}

// csrfOf is the token of the page's session.
export async function csrfOf(page: Page): Promise<string> {
  const res = await page.request.get('/api/session')
  expect(res.status()).toBe(200)
  return ((await res.json()) as { csrf: string }).csrf
}

// rowsOf are the rows of a table that hold data. On a phone a table has no
// row of headers, and each of its rows is a card whose cells say what they
// are, so a row is found by what it holds, not by its name.
export const rowsOf = (table: Locator) => table.getByRole('row').filter({ has: table.page().getByRole('cell') })

// onPhone says whether the window is as narrow as a phone's, where the
// pages lay themselves out for one.
export const onPhone = (viewport: { width: number } | null) => (viewport?.width ?? 0) < 720

// The live status of the top bar, in either width: the five pills, or the
// one and the list behind it.
export const statusOf = (page: Page) => page.getByRole('group', { name: 'Status of pco' })

// stateOf is the state of a scenario of the fake daemon that has one, its
// last cycle just finished: the state to change for a control.
export function stateOf(scenario: string): State {
  const file = join(repo, 'internal/api/apifake/testdata/scenarios', scenario, 'state.json')
  const st = JSON.parse(readFileSync(file, 'utf8')) as State
  const now = Date.now()
  st.at = new Date(now - 2000).toISOString()
  st.finishedAt = new Date(now).toISOString()
  return st
}

export const populated = (): State => stateOf('populated')
