import { useSyncExternalStore } from 'react'

// Whether the admin set the setup aside is kept in this browser only: another
// browser, or another admin, is shown the setup again.
export const dismissedKey = 'pco.setup.dismissed'

const listeners = new Set<() => void>()

// Where the choice lives when the browser keeps no storage: this page only.
let kept = false

function read(): boolean {
  try {
    return window.localStorage.getItem(dismissedKey) !== null
  } catch {
    return kept
  }
}

export function setDismissed(on: boolean): void {
  kept = on
  try {
    if (on) window.localStorage.setItem(dismissedKey, '1')
    else window.localStorage.removeItem(dismissedKey)
  } catch {
    // kept for this page only
  }
  for (const l of listeners) l()
}

function follow(listener: () => void): () => void {
  listeners.add(listener)
  window.addEventListener('storage', listener)
  return () => {
    listeners.delete(listener)
    window.removeEventListener('storage', listener)
  }
}

export function useSetupDismissed(): boolean {
  return useSyncExternalStore(follow, read)
}
