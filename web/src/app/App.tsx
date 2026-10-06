import './shell.css'

import { useEffect } from 'react'

import { type AppStore, appStore, StoreProvider, useApp } from '../api/store'
import { Skeleton } from '../components/Skeleton'
import { ToastProvider } from '../components/Toast'
import { SignIn } from '../pages/SignIn'
import { navigate, useLocation, useView } from './router'
import { Shell } from './Shell'

// loadPart loads a part of the page that is a chunk of its own. One that does
// not load was built for another version of pco, which has replaced this
// one: the page asks to be reloaded (spec-ui 3.1).
export function loadPart<T>(load: () => Promise<T>, store: AppStore = appStore): Promise<T> {
  return load().catch((e: unknown) => {
    store.markSkew()
    throw e
  })
}

// nextOf is where to go after the sign-in: a path of this page, nothing else.
export function nextOf(search: string): string {
  const next = new URLSearchParams(search).get('next') ?? ''
  return next.startsWith('/') && !next.startsWith('//') && !next.startsWith('/\\') && !next.startsWith('/signin') ? next : '/'
}

function Root() {
  const auth = useApp((s) => s.auth)
  const view = useView()
  const location = useLocation()

  useEffect(() => {
    if (auth === 'signed-out' && view.name !== 'signin') {
      navigate(`/signin?next=${encodeURIComponent(location)}`, true)
    } else if (auth === 'signed-in' && view.name === 'signin') {
      navigate(nextOf(new URL(location, 'https://page.invalid').search), true)
    }
  }, [auth, view.name, location])

  if (auth === 'loading') {
    return (
      <main id="main" className="loading">
        <Skeleton lines={4} label="Signing in" />
      </main>
    )
  }
  if (auth !== 'signed-in' || view.name === 'signin') {
    return <SignIn onSignedIn={() => navigate(nextOf(new URL(location, 'https://page.invalid').search), true)} />
  }
  return <Shell view={view} />
}

// App is the page: it signs in as far as the browser can by itself, then
// follows the daemon.
export function App({ store = appStore }: { store?: AppStore }) {
  useEffect(() => {
    void store.start()
  }, [store])
  return (
    <StoreProvider store={store}>
      <ToastProvider>
        <Root />
      </ToastProvider>
    </StoreProvider>
  )
}
