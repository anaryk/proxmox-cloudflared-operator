import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../api/store'
import type { State } from '../api/types.gen'
import cluster from '../fixtures/cluster.json'
import populated from '../fixtures/populated.json'
import untagged from '../fixtures/untagged.json'
import { appState, fakeStore } from '../test/store'
import { Banners, bannersOf } from './Banners'

const now = Date.parse('2026-10-01T12:05:00Z')
const st = populated as unknown as State

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(now)
})

afterEach(() => {
  vi.useRealTimers()
})

const keys = (s: Parameters<typeof bannersOf>[0]) => bannersOf(s).map((b) => b.key)

describe('the banners, most severe first', () => {
  test('every banner at once', () => {
    const state: State = {
      ...st,
      mode: 'observe',
      complete: false,
      writerVerdict: 'foreign',
      credentials: [],
    }
    const s = appState({
      state,
      conn: 'web-down',
      connSince: '2026-10-01T12:01:30Z',
      upstream: { up: false, since: '2026-10-01T12:01:30Z' },
      link: { state: 'too-many', since: '2026-10-01T12:01:00Z', retryAfter: 30 },
      skew: true,
    })
    expect(keys(s)).toEqual(['web', 'egress', 'writer', 'tabs', 'hold', 'inventory', 'waiting', 'approval', 'observe', 'credential', 'version'])
    // the daemon's banner when the page reaches pco web
    expect(keys({ ...s, conn: 'daemon-down', connSince: s.upstream.since })[0]).toBe('daemon')
  })

  test('the populated fixture', () => {
    expect(keys(appState({ state: st }))).toEqual(['egress', 'hold', 'waiting', 'approval'])
  })

  test('the banner of the missing token is left off the setup page, which is where it points', () => {
    const s = appState({ state: { ...st, credentials: [] } })
    expect(keys(s)).toContain('credential')
    expect(bannersOf(s, 0, 'setup').map((b) => b.key)).not.toContain('credential')
    expect(bannersOf(s, 0, 'routes').map((b) => b.key)).toContain('credential')
  })

  test('nothing for a state in order', () => {
    expect(keys(appState({ state: untagged as unknown as State }))).toEqual([])
  })

  test('the cluster fixture: the controller moved, and an address is blocked', () => {
    expect(keys(appState({ state: cluster as unknown as State }))).toEqual(['blocks', 'waiting', 'approval', 'moved'])
  })

  test('every banner of a cluster at once, in its place', () => {
    const base = cluster as unknown as State
    const plain = untagged as unknown as State
    const state: State = {
      ...plain,
      node: 'pve1',
      problems: ['waiting for the agents of pve2 to report to this controller; nothing is written at Cloudflare for up to 1m0s'],
      lease: { holder: 'pve1', previous: 'pve2', generation: 7, acquiredAt: base.lease?.acquiredAt, refreshAge: 0, valid: true, fenced: 'the quorum is not known', behind: 'the agents follow epoch 9', handoff: 'pve2' },
      cluster: {
        controller: 'pve1',
        generation: 7,
        members: ['pve1', 'pve2', 'pve3'],
        quorate: false,
        quorumDevice: false,
        coverage: { covered: false, since: '2026-10-01T11:50:00Z', uncovered: ['pve2'], flapping: ['pve2'] },
        upgrade: { from: '1.3.2', since: '2026-09-29T12:00:00Z', older: ['pve3'] },
        caRotation: { node: 'pve3', startedAt: '2026-10-01T11:50:00Z', done: ['credentials'], next: 'proxmox' },
        blocks: base.cluster?.blocks,
      },
    }
    expect(keys(appState({ state }))).toEqual(['quorum', 'fenced', 'behind', 'takeover', 'coverage', 'rotation', 'blocks', 'moved', 'handoff', 'upgrade'])
    const texts = bannersOf(appState({ state })).map((b) => render(<>{b.text}</>).container.textContent ?? '')
    expect(texts[0]).toBe('The cluster is not quorate: nothing is written until it is.')
    expect(texts[1]).toBe('The lease is fenced: the quorum is not known. Nothing is written.')
    expect(texts[2]).toContain('behind what the agents remember: the agents follow epoch 9')
    expect(texts[2]).toContain('pco promote')
    expect(texts[3]).toContain('waiting for the agents of pve2')
    expect(texts[4]).toMatch(/^Node pve2 has no pco agent with a running watch since .+: routes of other nodes' guests are pinned to their nodes\. The watch of pve2 broke twice/)
    expect(texts[5]).toContain('running for node pve3: 1 of 6 steps done, waiting on proxmox')
    expect(texts[6]).toContain('10.0.0.60 is blocked on every node')
    expect(texts[7]).toMatch(/^The controller moved from pve2 to pve1, generation 7, at /)
    expect(texts[8]).toContain('handed off to node pve2')
    expect(texts[9]).toContain('An upgrade from release 1.3.2 is unfinished')
    expect(texts[9]).toContain('still older: pve3')

    const pinned = { ...state, cluster: state.cluster && { ...state.cluster, coverage: { covered: false, uncovered: ['pve2'], pinned: true } } }
    expect(keys(appState({ state: pinned }))).not.toContain('coverage')
  })

  test('a node that does not hold the lease names the controller, or that none is known', () => {
    const plain = untagged as unknown as State
    const other = (holder: string) => appState({ state: { ...plain, node: 'pve2', lease: { holder, address: holder && '10.10.0.1', generation: 7, refreshAge: 0, valid: false } } })
    expect(keys(other('pve1'))).toEqual(['controller'])
    const [b] = bannersOf(other('pve1'))
    expect(render(<>{b?.text}</>).container.textContent).toContain('node pve1 (10.10.0.1) is the controller')
    const [none] = bannersOf(other(''))
    expect(render(<>{none?.text}</>).container.textContent).toContain('No controller is known')
    // the holder itself has no such banner, also while its lease is fenced
    expect(keys(appState({ state: { ...plain, node: 'pve1', lease: { holder: 'pve1', generation: 7, refreshAge: 0, valid: false, fenced: 'x' } } }))).toEqual(['fenced'])
  })

  test('stale data says from when, and how long ago on the browser\'s clock', () => {
    const s = appState({
      state: untagged as unknown as State,
      conn: 'stale',
      connSince: '2026-10-01T12:00:02Z',
      times: { at: '2026-10-01T12:00:00Z', finishedAt: '2026-10-01T12:00:02Z', digest: 'd' },
      receivedAt: 1000,
    })
    expect(keys(s)).toEqual(['stale'])
    const [b] = bannersOf(s, 41_000)
    const { container } = render(<>{b?.text}</>)
    expect(container.textContent).toMatch(/^The data is from .+, 40 s ago: no cycle of the daemon has finished since\./)
  })
})

test('the egress banner carries the command, to run as root', async () => {
  const { store } = await fakeStore({ state: populated })
  render(
    <StoreProvider store={store}>
      <Banners />
    </StoreProvider>,
  )
  const egress = document.querySelector('[data-banner="egress"]')
  expect(egress?.textContent).toContain('The egress filter is switched off since')
  expect(egress?.textContent).toContain('the connectors are not confined.')
  expect(egress?.querySelector('code')?.textContent).toBe('pco egress on')
  expect(egress?.textContent).toContain('Run it as root on the node.')
  expect(egress?.getAttribute('role')).toBe('alert')
})

test('the egress filter not loaded asks for pco egress load', () => {
  const s = appState({ state: { ...st, egress: { state: 'not loaded' } } })
  const [b] = bannersOf(s)
  const { container } = render(<>{b?.text}</>)
  expect(container.querySelector('code')?.textContent).toBe('pco egress load')
})

test('the reload banner after a hello of another version', async () => {
  const { store } = await fakeStore({ state: untagged })
  render(
    <StoreProvider store={store}>
      <Banners />
    </StoreProvider>,
  )
  expect(screen.queryByText('pco was updated; reload to use it.')).toBeNull()
  const hello = store.get().hello
  if (!hello) throw new Error('no hello')
  store.notice({ kind: 'hello', data: { ...hello, version: 'v9.9.9' } })
  expect(await screen.findByText('pco was updated; reload to use it.')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Reload' })).toBeTruthy()
})
