// Where an install stands on the way from nothing to a first published
// hostname, read from the state alone. The wizard shows these marks and opens
// the first step that is waiting for the admin.

import type { CredentialView, State } from '../../api/types.gen'
import { credentialState } from '../../text/words'
import { unset } from '../routes/routes'

export type StepId = 'install' | 'token' | 'zones' | 'reach' | 'route' | 'publish'

// done; todo, for the admin; wait, for the daemon; problem, what was done
// does not work; optional; blocked, until an earlier step is done.
export type Mark = 'done' | 'todo' | 'wait' | 'problem' | 'optional' | 'blocked'

export interface StepInfo {
  id: StepId
  title: string
  mark: Mark
}

export type TokenState = 'none' | 'problem' | 'unknown' | 'usable'
export type ZonesState = 'blocked' | 'waiting' | 'empty' | 'listed'
export type Missing = 'token' | 'route'

export interface Progress {
  // the daemon cannot run before the install command ran on the node
  installNeeded: boolean
  token: TokenState
  // a usable token whose last check did not write
  writeUntried: boolean
  zones: ZonesState
  routes: number
  // publishing was started: the daemon is out of observe-only mode
  publishing: boolean
  missing: Missing[]
  steps: StepInfo[]
  // the first step for the admin to act on
  current: StepId
}

// What the body of a step is given: the state, where it stands, and a way to
// open another step.
export interface StepProps {
  st: State
  p: Progress
  go: (id: StepId) => void
}

// The daemon says "pco setup" in every problem that only the install command
// on the node can solve.
const setupLine = 'pco setup'

// cycleAtOf is when the last cycle the page knows of began. The state is read
// again only when its digest changes, and the digest leaves the times out, so
// the notices of the stream, which carry them, can be later than the state.
export function cycleAtOf(state: Pick<State, 'at'> | undefined, noticed: string | undefined): string | undefined {
  if (unset(noticed)) return state?.at
  if (unset(state?.at)) return noticed
  return Date.parse(noticed ?? '') > Date.parse(state?.at ?? '') ? noticed : state?.at
}

export const needsInstall = (st: Pick<State, 'problems' | 'writerVerdict'>): boolean =>
  st.problems.some((p) => p.includes(setupLine)) || st.writerVerdict === 'unknown'

function tokenOf(credentials: readonly CredentialView[]): TokenState {
  const states = credentials.map(credentialState)
  if (states.includes('usable')) return 'usable'
  if (states.includes('problem')) return 'problem'
  return states.length > 0 ? 'unknown' : 'none'
}

// A cycle that began after the check of the credential has seen it.
function cycleSince(st: State, c: CredentialView): boolean {
  if (unset(st.at)) return false
  const checked = c.report?.checkedAt
  if (unset(checked)) return true
  return Date.parse(st.at ?? '') > Date.parse(checked ?? '')
}

function zonesOf(st: State, usable: readonly CredentialView[]): ZonesState {
  if (st.zones.length > 0) return 'listed'
  if (usable.length === 0) return 'blocked'
  return usable.some((c) => cycleSince(st, c)) ? 'empty' : 'waiting'
}

const titles: Readonly<Record<StepId, string>> = {
  install: 'Install check',
  token: 'API token',
  zones: 'Zones',
  reach: 'Reaching guests',
  route: 'First route',
  publish: 'Start publishing',
}

const zoneMarks: Readonly<Record<ZonesState, Mark>> = { blocked: 'blocked', waiting: 'wait', empty: 'problem', listed: 'done' }

// progressOf reads where the setup stands. pending is a credential the admin
// has just added: the daemon's state holds it only after the cycle that the
// add asks for, and until then it counts.
export function progressOf(st: State, pending?: CredentialView): Progress {
  const installNeeded = needsInstall(st)
  const credentials = pending && !st.credentials.some((c) => c.id === pending.id) ? [...st.credentials, pending] : st.credentials
  const token = tokenOf(credentials)
  const usable = credentials.filter((c) => credentialState(c) === 'usable')
  const writeUntried = usable.some((c) => c.report !== undefined && !c.report.deep)
  const zones = zonesOf(st, usable)
  const routes = st.routes.length
  const publishing = !unset(st.at) && st.mode !== 'observe'
  const missing: Missing[] = []
  if (token !== 'usable') missing.push('token')
  if (routes === 0) missing.push('route')

  const marks: Record<StepId, Mark> = {
    install: 'problem',
    // a token Cloudflare did not answer for is checked again, not fixed
    token: token === 'usable' ? 'done' : token === 'none' || token === 'unknown' ? 'todo' : 'problem',
    zones: zoneMarks[zones],
    reach: 'optional',
    route: routes > 0 ? 'done' : 'todo',
    publish: publishing && token === 'usable' ? 'done' : missing.length === 0 ? 'todo' : 'blocked',
  }
  const order: StepId[] = ['install', 'token', 'zones', 'reach', 'route', 'publish']
  const steps = order.filter((id) => id !== 'install' || installNeeded).map((id) => ({ id, title: titles[id], mark: marks[id] }))
  const current = steps.find((s) => s.mark === 'todo' || s.mark === 'problem')?.id ?? 'publish'
  return { installNeeded, token, writeUntried, zones, routes, publishing, missing, steps, current }
}
