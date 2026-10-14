import { lazy, type ReactNode, Suspense } from 'react'

import { Skeleton } from '../components/Skeleton'
import { Untrusted } from '../components/Untrusted'
import { CredentialDetail } from '../pages/edge/CredentialDetail'
import { Credentials } from '../pages/edge/Credentials'
import { TunnelDetail } from '../pages/edge/TunnelDetail'
import { Tunnels } from '../pages/edge/Tunnels'
import { ZoneDetail } from '../pages/edge/ZoneDetail'
import { Zones } from '../pages/edge/Zones'
import { GuestDetail } from '../pages/guests/GuestDetail'
import { GuestsPage } from '../pages/guests/GuestsPage'
import { RoutesSection } from '../pages/routes/RoutesPage'
import { SetupPage, Start } from '../pages/setup/Wizard'
import { Head } from './Head'
import { Link } from './Link'
import { loadPart, Part } from './part'
import type { View } from './router'

// The pages that are not on the first view and have much code of their own
// are loaded when they are first shown.
const SettingsPage = lazy(() => loadPart(() => import('../pages/settings/SettingsPage')).then((m) => ({ default: m.SettingsPage })))
const DoctorPage = lazy(() => loadPart(() => import('../pages/doctor/DoctorPage')).then((m) => ({ default: m.DoctorPage })))
const EventsPage = lazy(() => loadPart(() => import('../pages/events/EventsPage')).then((m) => ({ default: m.EventsPage })))
const NetworksPage = lazy(() => loadPart(() => import('../pages/networks/NetworksPage')).then((m) => ({ default: m.NetworksPage })))
const NodesPage = lazy(() => loadPart(() => import('../pages/nodes/NodesPage')).then((m) => ({ default: m.NodesPage })))
const LocalAgent = lazy(() => loadPart(() => import('../pages/LocalAgent')).then((m) => ({ default: m.LocalAgent })))

// Later holds a page loaded when first shown. Each page has its own, by its
// key: a page that did not load is not taken for the next one.
function Later({ children }: { children: ReactNode }) {
  return (
    <Part what="This page">
      <Suspense fallback={<Skeleton lines={6} label="Loading the page" />}>{children}</Suspense>
    </Part>
  )
}

const titles: Readonly<Record<string, [string, string]>> = {
  guests: ['Guests', 'The guests that carry the tag, their approvals and the issues in their Notes.'],
  claims: ['Claims', 'Who holds each hostname, and who waits for it.'],
  credentials: ['Credentials', 'The Cloudflare API tokens pco uses.'],
  zones: ['Zones', 'The zones the credentials list, and which credential serves each.'],
  tunnels: ['Tunnels', 'The tunnel of the install in each account, and its connectors.'],
  settings: ['Settings', 'The settings of the daemon.'],
}

// titleOf is the title of a view. What comes from the address is anybody's
// text, as a shared link can carry anything: it is shown as untrusted.
function titleOf(v: View): [ReactNode, ReactNode] {
  switch (v.name) {
    case 'guest':
      return [`${v.kind}/${v.vmid}`, '']
    case 'credential':
      return [
        <>
          Credential <Untrusted text={v.id} />
        </>,
        '',
      ]
    case 'zone':
      return [<Untrusted key="t" text={v.zone} hostname />, '']
    case 'tunnel':
      return [
        <>
          Tunnel in account <Untrusted text={v.account} />
        </>,
        '',
      ]
  }
  return titles[v.name] ?? ['', '']
}

// Body is what a view shows under its head.
function Body({ view }: { view: View }) {
  switch (view.name) {
    case 'guests':
      return <GuestsPage />
    case 'claims':
      return <GuestsPage claims />
    case 'guest':
      return <GuestDetail guest={`${view.kind}/${view.vmid}`} variant="page" />
    case 'credentials':
      return <Credentials />
    case 'credential':
      return <CredentialDetail id={view.id} />
    case 'zones':
      return <Zones />
    case 'zone':
      return <ZoneDetail zone={view.zone} variant="page" />
    case 'tunnels':
      return <Tunnels />
    case 'tunnel':
      return <TunnelDetail accountId={view.account} tunnel={view.tunnel} variant="page" />
  }
  return <p className="muted">This page is not part of this build yet.</p>
}

// Page is the content of a view.
export function Page({ view }: { view: View }) {
  switch (view.name) {
    case 'overview':
      return <Start />
    case 'setup':
      return <SetupPage />
    case 'networks':
      return (
        <Later key="networks">
          <NetworksPage />
        </Later>
      )
    case 'nodes':
    case 'node':
      return (
        <Later key="nodes">
          <NodesPage view={view} />
        </Later>
      )
    case 'agent':
      return (
        <Later key="agent">
          <LocalAgent />
        </Later>
      )
    case 'events':
      return (
        <Later key="events">
          <EventsPage />
        </Later>
      )
    case 'doctor':
      return (
        <Later key="doctor">
          <DoctorPage />
        </Later>
      )
    case 'settings': {
      const [title, description] = titleOf(view)
      return (
        <>
          <Head
            title={title}
            description={description}
            actions={
              <Link to="/setup" className="btn">
                First-run setup
              </Link>
            }
          />
          <Later key="settings">
            <SettingsPage />
          </Later>
        </>
      )
    }
    case 'routes':
    case 'route':
    case 'plan':
    case 'manual-new':
    case 'manual':
      return <RoutesSection view={view} />
    case 'not-found':
      return (
        <>
          <Head title="Not found" description="pco has no page at this address." />
          <p>
            <Link to="/">Go to the Overview</Link>
          </p>
        </>
      )
  }
  const [title, description] = titleOf(view)
  return (
    <>
      <Head title={title} description={description} />
      <Body view={view} />
    </>
  )
}
