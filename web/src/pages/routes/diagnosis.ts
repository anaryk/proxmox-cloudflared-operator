// The diagnoses run in this browser. The daemon keeps no result, and no other
// user sees one: the last of each hostname is kept here, in memory, with the
// digest of the state it ran against and the time, so that it can say when
// the state has changed since. One runs at a time, as the web process allows
// one at a time per user.

import { useSyncExternalStore } from 'react'

import type { Step } from '../../api/types.gen'

export interface Kept {
  steps: readonly Step[]
  digest: string // of the state the page had when the run began
  at: string
}

export interface Diagnoses {
  kept: ReadonlyMap<string, Kept>
  failed: ReadonlyMap<string, unknown>
  running?: { hostname: string; since: number }
}

// The hostnames kept at most; the oldest result goes first.
export const maxKept = 200

export class DiagnosisStore {
  #now: () => number
  #s: Diagnoses = { kept: new Map(), failed: new Map() }
  #listeners = new Set<() => void>()

  constructor(now: () => number = Date.now) {
    this.#now = now
  }

  get = (): Diagnoses => this.#s

  subscribe = (l: () => void): (() => void) => {
    this.#listeners.add(l)
    return () => this.#listeners.delete(l)
  }

  #set(patch: Partial<Diagnoses>): void {
    this.#s = { ...this.#s, ...patch }
    for (const l of this.#listeners) l()
  }

  // run diagnoses hostname with call, unless a diagnosis runs already. The
  // result, or the error, is kept for the hostname.
  async run(hostname: string, digest: string, call: () => Promise<readonly Step[]>): Promise<void> {
    if (this.#s.running) return
    const failed = new Map(this.#s.failed)
    failed.delete(hostname)
    this.#set({ running: { hostname, since: this.#now() }, failed })
    try {
      const steps = await call()
      const kept = new Map(this.#s.kept)
      kept.delete(hostname)
      kept.set(hostname, { steps, digest, at: new Date(this.#now()).toISOString() })
      for (const oldest of kept.keys()) {
        if (kept.size <= maxKept) break
        kept.delete(oldest)
      }
      this.#set({ kept, running: undefined })
    } catch (e) {
      this.#set({ failed: new Map(this.#s.failed).set(hostname, e), running: undefined })
    }
  }

  clear(): void {
    this.#set({ kept: new Map(), failed: new Map(), running: undefined })
  }
}

export const diagnoses = new DiagnosisStore()

export function useDiagnoses(store: DiagnosisStore = diagnoses): Diagnoses {
  return useSyncExternalStore(store.subscribe, store.get)
}

// changedSince says whether the state has moved on from the one a kept
// result ran against.
export function changedSince(kept: Kept, digest: string | undefined): boolean {
  return digest !== undefined && kept.digest !== digest
}
