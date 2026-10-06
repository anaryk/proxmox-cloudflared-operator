import type { ReactNode } from 'react'

import { Untrusted } from '../components/Untrusted'
import { Overview } from '../pages/Overview'
import { DoctorPage } from '../pages/doctor/DoctorPage'
import { CredentialDetail } from '../pages/edge/CredentialDetail'
import { Credentials } from '../pages/edge/Credentials'
import { TunnelDetail } from '../pages/edge/TunnelDetail'
import { Tunnels } from '../pages/edge/Tunnels'
import { ZoneDetail } from '../pages/edge/ZoneDetail'
import { Zones } from '../pages/edge/Zones'
import { EventsPage } from '../pages/events/EventsPage'
import { GuestDetail } from '../pages/guests/GuestDetail'
import { GuestsPage } from '../pages/guests/GuestsPage'
import { NetworksPage } from '../pages/networks/NetworksPage'
import { RoutesSection } from '../pages/routes/RoutesPage'
import { SettingsPage } from '../pages/settings/SettingsPage'
import { Head } from './Head'
import { Link } from './Link'
import type { View } from './router'

const titles: Readonly<Record<string, [string, string]>> = {
  guests: ['Guests', 'The guests that carry the tag, their approvals and the issues in their Notes.'],
  claims: ['Claims', 'Who holds each hostname, and who waits for it.'],
  credentials: ['Credentials', 'The Cloudflare API tokens pco uses.'],
  zones: ['Zones', 'The zones the credentials list, and which credential serves each.'],
  tunnels: ['Tunnels', 'The tunnel of the install in each account, and its connectors.'],
  settings: ['Settings', 'The settings of the daemon.'],
  setup: ['First-run setup', 'A token, the zones, and the first route.'],
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
      return <TunnelDetail accountId={view.account} variant="page" />
  }
  return <p className="muted">This page is not part of this build yet.</p>
}

// Page is the content of a view.
export function Page({ view }: { view: View }) {
  switch (view.name) {
    case 'overview':
      return <Overview />
    case 'networks':
      return <NetworksPage />
    case 'events':
      return <EventsPage />
    case 'doctor':
      return <DoctorPage />
    case 'settings': {
      const [title, description] = titleOf(view)
      return (
        <>
          <Head title={title} description={description} />
          <SettingsPage />
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
