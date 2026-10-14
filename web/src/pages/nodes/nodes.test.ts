import { describe, expect, test } from 'vitest'

import type { NodeView, State } from '../../api/types.gen'
import cluster from '../../fixtures/cluster.json'
import populated from '../../fixtures/populated.json'
import {
  clustered,
  compareNodes,
  fingerprintIssue,
  fingerprintOf,
  kindText,
  moveText,
  nodeNameIssue,
  nodeNeeds,
  nodesNeeding,
  nodeTone,
  notController,
  placementText,
  replicaLevelText,
  replicaTone,
  skewText,
  takeoverHold,
  targetsText,
} from './nodes'

const st = cluster as unknown as State
const alone = populated as unknown as State

const node = (patch: Partial<NodeView>): NodeView => ({
  name: 'pve9',
  state: 'active',
  kind: 'host',
  controller: false,
  statusAge: 0,
  watchRunning: true,
  covered: true,
  replicaReady: true,
  verified: 0,
  failing: 0,
  belowMinimum: 0,
  pinned: 0,
  clockSkew: 0,
  ...patch,
})

describe('a cluster is shown only where there is one', () => {
  test('the cluster fixture, and a node alone', () => {
    expect(clustered(st)).toBe(true)
    expect(clustered(alone)).toBe(false)
    expect(clustered(undefined)).toBe(false)
  })

  test('a node that does not hold the lease is on a cluster, whoever holds it', () => {
    const other: State = { ...alone, lease: { holder: 'pve2', generation: 7, refreshAge: 0, valid: false } }
    expect(notController(other)).toBe(true)
    expect(clustered(other)).toBe(true)
    const nobody: State = { ...alone, lease: { holder: '', generation: 0, refreshAge: 0, valid: false } }
    expect(notController(nobody)).toBe(true)
    // the holder itself, also while its lease is not valid for a moment
    expect(notController({ ...alone, lease: { holder: 'pve1', generation: 7, refreshAge: 0, valid: true } })).toBe(false)
    expect(notController({ ...alone, lease: { holder: 'pve1', generation: 7, refreshAge: 0, valid: false, fenced: 'quorum lost' } })).toBe(false)
    expect(notController(st)).toBe(false)
  })
})

describe('the nodes', () => {
  test('are ordered as pco node list orders them: by state, then by name', () => {
    const names = [node({ name: 'b', state: 'removed' }), node({ name: 'c', state: 'joining' }), node({ name: 'b' }), node({ name: 'a' })].sort(compareNodes).map((n) => n.name)
    expect(names).toEqual(['a', 'b', 'c', 'b'])
  })

  test('those that need a person, and the count of the navigation', () => {
    expect(['joining', 'contested', 'unknown'].map((state) => nodeNeeds({ state }))).toEqual([true, true, true])
    expect(['active', 'draining', 'down', 'removed'].map((state) => nodeNeeds({ state }))).toEqual([false, false, false, false])
    expect(nodesNeeding(st)).toBe(1)
    expect(nodesNeeding(alone)).toBe(0)
    const cluster = st.cluster ?? { controller: 'pve1', generation: 7, members: [], quorate: true, quorumDevice: false, coverage: { covered: true } }
    const refused: State = { ...st, cluster: { ...cluster, caRotation: { node: 'pve3', done: [], finishedAt: '2026-10-01T12:00:00Z', refused: ['pve2'] } } }
    expect(nodesNeeding(refused)).toBe(2)
  })

  test('the colour of each state, never alone', () => {
    expect(['active', 'joining', 'draining', 'unknown', 'down', 'contested', 'removed', 'odd'].map(nodeTone)).toEqual(['ok', 'info', 'info', 'warn', 'fail', 'fail', 'idle', 'idle'])
  })

  test('what runs the agent, the clock skew and the targets', () => {
    expect(kindText({ kind: 'host' })).toBe('host')
    expect(kindText({ kind: 'appliance', vmid: 9301 })).toBe('appliance, container 9301')
    expect(kindText({ kind: 'appliance' })).toBe('appliance')
    expect(skewText(0)).toBe('none')
    expect(skewText(300e6)).toBe('+300 ms (ahead)')
    expect(skewText(-2.5e9)).toBe('-3 s (behind)')
    expect(skewText(90e9)).toBe('+1 min (ahead)')
    expect(targetsText({ verified: 4, failing: 0, belowMinimum: 0, pinned: 0 })).toBe('4 verified')
    expect(targetsText({ verified: 10, failing: 1, belowMinimum: 2, pinned: 1 })).toBe('10 verified · 1 failing · 2 below the minimum · 1 pinned')
  })
})

describe('what is typed', () => {
  test('a fingerprint is hex, at least 8 and at most 64 digits, colons allowed', () => {
    expect(fingerprintIssue('')).toBeUndefined()
    expect(fingerprintIssue('0123abcd')).toBeUndefined()
    expect(fingerprintIssue('01:23:AB:CD')).toBeUndefined()
    expect(fingerprintIssue('0123abc')).toMatch(/at least 8/)
    expect(fingerprintIssue('zz0123abcd')).toMatch(/digits 0-9 and a-f/)
    expect(fingerprintIssue('0'.repeat(65))).toMatch(/64 hex digits/)
    expect(fingerprintOf(' 01:23:AB:CD ')).toBe('0123abcd')
  })

  test('a node name is a label of a host name', () => {
    expect(nodeNameIssue('pve-lab-1')).toBeUndefined()
    expect(nodeNameIssue('')).toBe('a node is named')
    expect(nodeNameIssue('PVE1')).toMatch(/lower-case/)
    expect(nodeNameIssue('pve1.example.com')).toMatch(/lower-case/)
    expect(nodeNameIssue('-pve')).toMatch(/lower-case/)
  })
})

describe('the words of a replica, a placement and a move', () => {
  test('port (attested) only where the hosting node vouched', () => {
    expect(replicaLevelText({ level: 'port', attested: true })).toBe('port (attested)')
    expect(replicaLevelText({ level: 'port' })).toBe('port')
    expect(replicaLevelText({})).toBe('-')
    expect(replicaTone({ ok: true, inSet: true })).toBe('ok')
    expect(replicaTone({ ok: true, inSet: false })).toBe('warn')
    expect(replicaTone({ ok: false, inSet: false })).toBe('fail')
  })

  test('the placement and the move', () => {
    expect(placementText({ tunnel: 'pco-abc123', kind: 'shared' })).toBe('shared tunnel pco-abc123')
    expect(placementText({ tunnel: 'pco-abc123-pve2', kind: 'pinned', node: 'pve2', why: 'pve1 does not reach it while pve2 does' })).toBe(
      'pinned to pve2 (tunnel pco-abc123-pve2): pve1 does not reach it while pve2 does',
    )
    expect(moveText({ from: 'pco-abc123-pve2', to: 'pco-abc123', step: 'rolled-out' })).toBe(
      'moving from pco-abc123-pve2 to pco-abc123: its connectors run the configuration with them',
    )
    expect(moveText({ from: 'a', to: 'b', step: 'odd' })).toBe('moving from a to b: odd')
  })

  test('the hold of a takeover is read from the problems', () => {
    expect(takeoverHold(st)).toBeUndefined()
    const line = 'waiting for the agents of pve2 to report to this controller; nothing is written at Cloudflare for up to 1m0s'
    expect(takeoverHold({ problems: ['other', line] })).toBe(line)
  })
})
