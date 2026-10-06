// History routing over the paths of View. pco web answers index.html for
// exactly these paths, so each view has an address to share.

import { useSyncExternalStore } from 'react'

export type View =
  | { name: 'overview' }
  | { name: 'routes' }
  | { name: 'route'; hostname: string; owner?: string }
  | { name: 'plan' }
  | { name: 'manual-new' }
  | { name: 'manual'; id: string }
  | { name: 'guests' }
  | { name: 'guest'; kind: 'qemu' | 'lxc'; vmid: number }
  | { name: 'claims' }
  | { name: 'networks' }
  | { name: 'credentials' }
  | { name: 'credential'; id: string }
  | { name: 'zones' }
  | { name: 'zone'; zone: string }
  | { name: 'tunnels' }
  | { name: 'tunnel'; account: string }
  | { name: 'events' }
  | { name: 'doctor' }
  | { name: 'settings' }
  | { name: 'setup' }
  | { name: 'signin' }
  | { name: 'not-found' }

// The part of a path between two slashes, decoded; undefined when it cannot
// be or would name another path.
function part(raw: string | undefined): string | undefined {
  if (!raw) return undefined
  try {
    const v = decodeURIComponent(raw)
    return v === '' || v.includes('/') ? undefined : v
  } catch {
    return undefined
  }
}

// match is the view of a path and its query.
export function match(pathname: string, search = ''): View {
  const query = new URLSearchParams(search)
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, '') : pathname
  const [, first, second, third, fourth, ...more] = path.split('/')
  const none: View = { name: 'not-found' }
  if (more.length > 0) return none
  switch (first) {
    case '':
      return second === undefined ? { name: 'overview' } : none
    case 'routes':
      if (second === undefined) return { name: 'routes' }
      if (second === 'plan' && third === undefined) return { name: 'plan' }
      if (second === 'manual') {
        if (third === 'new' && fourth === undefined) return { name: 'manual-new' }
        const id = part(third)
        return id && fourth === undefined ? { name: 'manual', id } : none
      }
      {
        const hostname = part(second)
        if (!hostname || third !== undefined) return none
        const owner = query.get('owner') ?? undefined
        return owner ? { name: 'route', hostname, owner } : { name: 'route', hostname }
      }
    case 'guests':
      if (second === undefined) return { name: 'guests' }
      if (second === 'claims' && third === undefined) return { name: 'claims' }
      if ((second === 'qemu' || second === 'lxc') && third !== undefined && /^\d{1,9}$/.test(third) && fourth === undefined) {
        return { name: 'guest', kind: second, vmid: Number(third) }
      }
      return none
    case 'networks':
      return second === undefined ? { name: 'networks' } : none
    case 'edge': {
      if (fourth !== undefined) return none
      const id = part(third)
      switch (second) {
        case 'credentials':
          return third === undefined ? { name: 'credentials' } : id ? { name: 'credential', id } : none
        case 'zones':
          return third === undefined ? { name: 'zones' } : id ? { name: 'zone', zone: id } : none
        case 'tunnels':
          return third === undefined ? { name: 'tunnels' } : id ? { name: 'tunnel', account: id } : none
      }
      return none
    }
  }
  if (second !== undefined) return none
  switch (first) {
    case 'events':
      return { name: 'events' }
    case 'doctor':
      return { name: 'doctor' }
    case 'settings':
      return { name: 'settings' }
    case 'setup':
      return { name: 'setup' }
    case 'signin':
      return { name: 'signin' }
  }
  return none
}

// The item of the navigation a view belongs to.
export type Section = 'overview' | 'routes' | 'guests' | 'networks' | 'credentials' | 'zones' | 'tunnels' | 'events' | 'doctor' | 'settings'

export function sectionOf(v: View): Section | undefined {
  switch (v.name) {
    case 'overview':
      return 'overview'
    case 'routes':
    case 'route':
    case 'plan':
    case 'manual-new':
    case 'manual':
      return 'routes'
    case 'guests':
    case 'guest':
    case 'claims':
      return 'guests'
    case 'networks':
      return 'networks'
    case 'credentials':
    case 'credential':
      return 'credentials'
    case 'zones':
    case 'zone':
      return 'zones'
    case 'tunnels':
    case 'tunnel':
      return 'tunnels'
    case 'events':
      return 'events'
    case 'doctor':
      return 'doctor'
    case 'settings':
    case 'setup':
      return 'settings'
  }
  return undefined
}

const changed = new Set<() => void>()

function follow(listener: () => void): () => void {
  changed.add(listener)
  window.addEventListener('popstate', listener)
  return () => {
    changed.delete(listener)
    window.removeEventListener('popstate', listener)
  }
}

const here = () => window.location.pathname + window.location.search + window.location.hash

// navigate goes to a path of the page without loading it again. A fragment
// names a part of the view, such as the Problems card of the Overview, which
// is given the focus once the view is shown, as a browser would scroll to it.
export function navigate(to: string, replace = false): void {
  if (to !== here()) {
    if (replace) window.history.replaceState(null, '', to)
    else window.history.pushState(null, '', to)
    for (const l of changed) l()
  }
  const id = new URL(to, 'https://page.invalid').hash.slice(1)
  if (id) {
    setTimeout(() => {
      const name = part(id)
      if (name) document.getElementById(name)?.focus()
    }, 0)
  }
}

// useLocation is the path, query and fragment shown, as one string.
export function useLocation(): string {
  return useSyncExternalStore(follow, here)
}

export function useView(): View {
  const loc = useLocation()
  const url = new URL(loc, 'https://page.invalid')
  return match(url.pathname, url.search)
}
