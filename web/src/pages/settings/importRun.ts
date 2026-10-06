// The writes of an import, in order, and the report of how far they got.

import { ApiError } from '../../api/client'
import { explain } from '../../api/errors'
import type { ManualRouteView, Settings } from '../../api/types.gen'
import type { RoutePlan } from './diff'

export interface Calls {
  putSettings: (rev: number, settings: Settings) => Promise<unknown>
  createRoute: (route: ManualRouteView) => Promise<unknown>
  updateRoute: (route: ManualRouteView, rev: number) => Promise<unknown>
  deleteRoute: (id: string, rev: number) => Promise<unknown>
}

export interface Step {
  label: string
  run: () => Promise<unknown>
}

// importSteps are the writes that make the stored settings and routes what
// a file says: the settings first, so that the prefixes of the file are in
// force when the routes are checked against them; then the routes added, the
// routes changed at the revision stored, and last the routes deleted, so that
// none is lost before what replaces it is there. settings is left out when
// they did not change.
export function importSteps(o: { rev: number; settings?: Settings; plan: RoutePlan }, calls: Calls): Step[] {
  const steps: Step[] = []
  const { settings } = o
  if (settings) steps.push({ label: 'settings', run: () => calls.putSettings(o.rev, settings) })
  for (const r of o.plan.add) steps.push({ label: `add route ${r.id}`, run: () => calls.createRoute(r) })
  for (const u of o.plan.update) steps.push({ label: `change route ${u.after.id}`, run: () => calls.updateRoute(u.after, u.before.rev) })
  for (const r of o.plan.remove) steps.push({ label: `delete route ${r.id}`, run: () => calls.deleteRoute(r.id, r.rev) })
  return steps
}

export interface Outcome {
  saved: string[]
  failed?: {
    label: string
    text: string
    // The text is the daemon's, which may quote what a file or a guest said.
    quoted: boolean
    // The write got no answer: it may have happened.
    unknown: boolean
  }
  notSaved: string[]
}

// runSteps writes one step after the other and stops at the first that
// fails, whatever the reason: what follows may rest on it.
export async function runSteps(steps: readonly Step[]): Promise<Outcome> {
  const out: Outcome = { saved: [], notSaved: [] }
  for (const [at, step] of steps.entries()) {
    try {
      await step.run()
      out.saved.push(step.label)
    } catch (e) {
      if (e instanceof ApiError) {
        const said = explain(e)
        out.failed = { label: step.label, text: said.text, quoted: said.quoted, unknown: e.status === 0 }
      } else {
        out.failed = { label: step.label, text: e instanceof Error ? e.message : String(e), quoted: false, unknown: false }
      }
      out.notSaved = steps.slice(at + 1).map((s) => s.label)
      return out
    }
  }
  return out
}
