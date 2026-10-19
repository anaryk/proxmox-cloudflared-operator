import { render } from '@testing-library/react'
import type { JSX } from 'react'
import { expect, test } from 'vitest'

import { StoreProvider } from '../api/store'
import { stateFields, type State } from '../api/types.gen'
import { ToastProvider } from '../components/Toast'
import populated from '../fixtures/populated.json'
import { DoctorPage } from '../pages/doctor/DoctorPage'
import { NodesPage } from '../pages/nodes/NodesPage'
import { PlanSections } from '../pages/routes/PlanPage'
import { fakeStore } from '../test/store'
import { fieldHomes, homeless } from './fields'

test('every field of the state has a home on the page', () => {
  expect(homeless()).toEqual([])
})

test('a field the daemon adds has none until it is given one', () => {
  expect(homeless([...stateFields, 'later'])).toEqual(['later'])
})

test('no home is kept for a field the state no longer has', () => {
  expect(Object.keys(fieldHomes).filter((k) => !stateFields.includes(k))).toEqual([])
})

// The pages the tests render to check a home, by the words a home begins
// with.
const pages: Record<string, () => JSX.Element> = {
  'Doctor:': () => <DoctorPage />,
  'Routes > Plan:': () => <PlanSections />,
  'Nodes:': () => <NodesPage view={{ name: 'nodes' }} />,
}

// For each field whose home is one of those pages: a value of it that only
// it gives the state, and the text that shows the page shows it.
const probes: Record<string, { state: Partial<State>; shows: string }> = {
  identity: { state: { identity: { vmid: 9871, node: 'pve-probe', ok: false, why: 'not the probe', copy: false } }, shows: 'lxc/9871 on node pve-probe' },
  epochDrawnAt: { state: { epochDrawnAt: '2026-10-03T04:05:06Z' }, shows: '2026-10-03T04:05:06Z' },
  actions: {
    state: { actions: [{ kind: 'create-record', credentialId: 'cred1', target: 'probe-action.example.com', detail: '', destructive: false, applied: false }] },
    shows: 'probe-action.example.com',
  },
  conflicts: { state: { conflicts: [{ zone: 'example.com', name: 'probe-conflict.example.com', type: 'A', content: '192.0.2.99' }] }, shows: 'probe-conflict.example.com' },
  lost: { state: { lost: ['probe-lost.example.com'] }, shows: 'probe-lost.example.com' },
  lease: { state: { lease: { holder: 'pve-probe-holder', generation: 4242, refreshAge: 3e9, valid: true } }, shows: 'pve-probe-holder' },
  nodes: {
    state: {
      nodes: [
        { name: 'pve-probe-node', state: 'active', kind: 'host', controller: false, statusAge: 1e9, watchRunning: true, covered: true, replicaReady: true, verified: 1, failing: 0, belowMinimum: 0, pinned: 0, clockSkew: 0 },
      ],
    },
    shows: 'pve-probe-node',
  },
  cluster: {
    state: { cluster: { controller: 'pve-probe-controller', generation: 4242, members: ['pve-probe-controller'], quorate: true, quorumDevice: false, coverage: { covered: true } } },
    shows: 'pve-probe-controller',
  },
}

const checked = Object.entries(fieldHomes).filter(([, home]) => Object.keys(pages).some((page) => home.startsWith(page)))

test('the homes the tests can render are there to check', () => {
  expect(checked.map(([field]) => field).sort()).toEqual(['actions', 'cluster', 'conflicts', 'epochDrawnAt', 'identity', 'lease', 'lost', 'nodes'])
})

test.each(checked)('%s is shown where its home says: %s', async (field, home) => {
  const probe = probes[field]
  if (!probe) throw new Error(`no value to look for of ${field}: add one to probes`)
  const page = Object.entries(pages).find(([name]) => home.startsWith(name))?.[1]
  if (!page) throw new Error(`no page for ${home}`)
  const { store } = await fakeStore({ state: { ...(populated as unknown as State), ...probe.state } })
  const { container } = render(
    <StoreProvider store={store}>
      <ToastProvider>{page()}</ToastProvider>
    </StoreProvider>,
  )
  const shown = container.textContent?.includes(probe.shows) || container.querySelector(`time[datetime="${probe.shows}"]`) !== null
  expect(shown, `${field} is not on the page its home names`).toBe(true)
})
