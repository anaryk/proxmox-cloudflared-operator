import { useSyncExternalStore } from 'react'

// The look the user chose in this browser: the theme (absent: the system's)
// and the density of tables (absent: comfortable). tokens.css reads both from
// attributes of the root element.
export type Theme = 'system' | 'light' | 'dark'
export type Density = 'comfortable' | 'compact'

export interface Preferences {
  theme: Theme
  density: Density
}

export const rowHeights: Readonly<Record<Density, number>> = { comfortable: 34, compact: 28 }

const themeKey = 'pco.theme'
const densityKey = 'pco.density'

// The browser may refuse storage to the page, and even the property itself
// throws then; a choice is kept for as long as the page is open.
function read(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

function write(key: string, value: string | null): void {
  try {
    if (value === null) window.localStorage.removeItem(key)
    else window.localStorage.setItem(key, value)
  } catch {
    // kept for this page only
  }
}

function stored(): Preferences {
  const theme = read(themeKey)
  return {
    theme: theme === 'light' || theme === 'dark' ? theme : 'system',
    density: read(densityKey) === 'compact' ? 'compact' : 'comfortable',
  }
}

function apply(p: Preferences): void {
  const root = document.documentElement
  if (p.theme === 'system') delete root.dataset.theme
  else root.dataset.theme = p.theme
  if (p.density === 'compact') root.dataset.density = 'compact'
  else delete root.dataset.density
}

let current: Preferences = stored()
let following = false
const listeners = new Set<() => void>()

function update(p: Preferences): void {
  current = p
  apply(p)
  for (const listener of listeners) listener()
}

// startPreferences applies what is stored, before the first render, and
// follows the choices made in other tabs.
export function startPreferences(): void {
  update(stored())
  if (following) return
  following = true
  window.addEventListener('storage', (e) => {
    if (e.key === null || e.key === themeKey || e.key === densityKey) update(stored())
  })
}

export function preferences(): Preferences {
  return current
}

export function setTheme(theme: Theme): void {
  write(themeKey, theme === 'system' ? null : theme)
  update({ ...current, theme })
}

export function setDensity(density: Density): void {
  write(densityKey, density === 'compact' ? density : null)
  update({ ...current, density })
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function usePreferences(): Preferences {
  return useSyncExternalStore(subscribe, preferences)
}
