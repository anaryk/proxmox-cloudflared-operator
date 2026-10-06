import { useEffect, useState } from 'react'

import { api, ApiError } from '../../api/client'
import { useApp } from '../../api/store'
import type { RouteSeries, RouteView } from '../../api/types.gen'
import { Skeleton } from '../../components/Skeleton'
import { TrafficChart } from '../../components/TrafficChart'
import { Untrusted } from '../../components/Untrusted'
import { ErrorText } from './parts'
import { unset } from './routes'

export function sharedText(shared: number): string {
  return shared === 1 ? 'shared with 1 other route' : `shared with ${shared} other routes`
}

// RouteTraffic is the traffic of the target of a route: the connections the
// connector opened to its address and port, as the egress filter counts
// them, for the last 15 minutes. Routes on the same target share one
// figure, and the page says so. It is read again with each round of the
// traffic.
export function RouteTraffic({ route }: { route: RouteView }) {
  const why = useApp((s) => s.traffic?.routesWhy)
  const round = useApp((s) => s.traffic?.at)
  const [series, setSeries] = useState<RouteSeries | ApiError>()
  const counted = route.state === 'active' && !why

  useEffect(() => {
    if (!counted) return
    let on = true
    api<RouteSeries>('GET', `/api/v1/traffic/route?hostname=${encodeURIComponent(route.hostname)}`, undefined, { background: true }).then(
      (s) => on && setSeries(s),
      (e: unknown) => on && setSeries(e instanceof ApiError ? e : new ApiError(0, { code: 'internal', error: String(e) })),
    )
    return () => {
      on = false
    }
  }, [counted, route.hostname, round])

  if (why) {
    return (
      <p className="muted">
        No figures per route: <Untrusted text={why} />
      </p>
    )
  }
  if (!counted) return <p className="muted">Only an active route has a target the egress filter counts.</p>
  if (series === undefined) return <Skeleton lines={2} label="Loading the traffic of the route" />
  if (series instanceof ApiError) {
    if (series.code === 'not_found') return <p className="muted">The egress filter counts no target for this route yet.</p>
    return <ErrorText error={series} />
  }
  const samples = series.samples ?? []
  const last = samples.at(-1)?.at
  return (
    <div className="route-traffic">
      <TrafficChart
        label="Connections opened to the target"
        series={[{ name: 'connections opened', unit: '/s', samples: samples.map((s) => ({ at: s.at, value: s.flowsPerSec })) }]}
        end={!unset(round) ? (round ?? '') : (last ?? new Date().toISOString())}
      />
      <p className="muted">
        Target <Untrusted text={series.target} className="mono" />
        {series.shared > 0 && <>, {sharedText(series.shared)}: a hostname has no figure of its own, and these are the connections to all of them</>}
      </p>
    </div>
  )
}
