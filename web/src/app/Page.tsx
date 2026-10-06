import type { ReactNode } from 'react'

import { useApp } from '../api/store'
import { Skeleton } from '../components/Skeleton'
import { StateBadge } from '../components/StateBadge'
import { Untrusted } from '../components/Untrusted'
import { DoctorPage } from '../pages/doctor/DoctorPage'
import { EventsPage } from '../pages/events/EventsPage'
import { NetworksPage } from '../pages/networks/NetworksPage'
import { SettingsPage } from '../pages/settings/SettingsPage'
import { compareRouteStates } from '../text/words'
import { Head } from './Head'
import { Link } from './Link'
import { NoRoutes } from './NoRoutes'
import { Problems } from './Problems'
import type { View } from './router'

// Routes says how many routes there are in each state, or why there are none.
function RouteCounts() {
  const st = useApp((s) => s.state)
  const gateTag = useApp((s) => s.settings?.settings.gateTag)
  if (!st) return <Skeleton lines={3} label="Loading the state" />
  if (st.routes.length === 0) return <NoRoutes state={st} gateTag={gateTag} />
  const counts = new Map<string, number>()
  for (const r of st.routes) counts.set(r.state, (counts.get(r.state) ?? 0) + 1)
  return (
    <ul className="route-counts">
      {[...counts.keys()].sort(compareRouteStates).map((state) => (
        <li key={state}>
          <StateBadge state={state} /> <b className="num">{counts.get(state)}</b>
        </li>
      ))}
    </ul>
  )
}

function Overview() {
  const st = useApp((s) => s.state)
  return (
    <>
      <Head title="Overview" description="What pco publishes, and what needs you." />
      {st && <Problems state={st} />}
      <section className="card" aria-labelledby="overview-routes">
        <div className="card-head">
          <h2 id="overview-routes">Routes</h2>
        </div>
        <div className="card-body">
          <RouteCounts />
        </div>
      </section>
    </>
  )
}

const titles: Readonly<Record<string, [string, string]>> = {
  routes: ['Routes', 'Every hostname the guests and the manual routes ask for, and what pco made of it.'],
  plan: ['Plan', 'What the cycles would change at Cloudflare, and what waits for a confirmation.'],
  'manual-new': ['New manual route', 'A hostname for an address that no guest annotation names.'],
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
    case 'route':
      return [
        <Untrusted key="t" text={v.hostname} hostname />,
        v.owner ? (
          <>
            The route of <Untrusted text={v.owner} />.
          </>
        ) : (
          'The routes of this hostname.'
        ),
      ]
    case 'manual':
      return [
        <>
          Manual route <Untrusted text={v.id} />
        </>,
        '',
      ]
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
      {view.name === 'routes' && <RouteCounts />}
      {view.name !== 'routes' && <p className="muted">This page is not part of this build yet.</p>}
    </>
  )
}
