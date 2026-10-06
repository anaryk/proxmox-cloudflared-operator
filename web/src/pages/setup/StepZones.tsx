import type { ReactNode } from 'react'

import type { Exclusion, State, ZoneView } from '../../api/types.gen'
import { Link } from '../../app/Link'
import { Badge } from '../../components/Badge'
import { Busy } from '../../components/Busy'
import { Button } from '../../components/Button'
import { type Column, Table } from '../../components/Table'
import { Untrusted } from '../../components/Untrusted'
import { ZoneState } from '../edge/ZoneDetail'
import { zonePath } from '../edge/Zones'
import { accountName, credentialLabel } from '../kit'
import type { StepProps } from './steps'

// leftOut are the zones the credentials list but whose DNS they may not
// read, each with what would add it.
export function leftOut(st: Pick<State, 'credentials'>): Exclusion[] {
  const seen = new Set<string>()
  const out: Exclusion[] = []
  for (const c of st.credentials) {
    for (const x of c.report?.excluded ?? []) {
      const key = `${x.zoneId || x.zone}\u0000${x.reason}\u0000${x.detail}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push(x)
    }
  }
  return out
}

function LeftOut({ excluded }: { excluded: readonly Exclusion[] }) {
  if (excluded.length === 0) return null
  return (
    <>
      <h3 className="wizard-sub">Zones the token leaves out</h3>
      <ul className="plain-list">
        {excluded.map((x) => (
          <li key={`${x.zoneId || x.zone}\u0000${x.reason}`}>
            <Untrusted text={`${x.zone} left out: ${x.reason}`} />
            {x.detail && (
              <>
                {': '}
                <Untrusted text={x.detail} />
              </>
            )}
          </li>
        ))}
      </ul>
    </>
  )
}

// StepZones shows the zones as the daemon uses them. There is nothing to do
// in the common case; a zone that two credentials list needs a pin, which its
// page offers.
export function StepZones({ st, p, go }: StepProps): ReactNode {
  const excluded = leftOut(st)
  const routes = new Map<string, number>()
  for (const r of st.routes) if (r.zone) routes.set(r.zone, (routes.get(r.zone) ?? 0) + 1)

  const columns: Column<ZoneView>[] = [
    {
      key: 'zone',
      header: 'Zone',
      lead: true,
      cell: (z) => (
        <Link to={zonePath(z.name)}>
          <Untrusted text={z.name} hostname />
        </Link>
      ),
    },
    {
      key: 'state',
      header: 'State',
      cell: (z) => (
        <>
          <ZoneState state={z.state} />
          {z.credentials.length > 1 && !z.pinned && (
            <>
              {' '}
              <Badge tone="warn">needs a pin</Badge>
            </>
          )}
        </>
      ),
    },
    { key: 'account', header: 'Account', cell: (z) => <Untrusted text={accountName(st, z.accountId)} /> },
    { key: 'listed', header: 'Listed by', cell: (z) => <Untrusted text={z.credentials.map((id) => credentialLabel(st, id)).join(', ') || '-'} /> },
    { key: 'routes', header: 'Routes', className: 'num', cell: (z) => routes.get(z.name) ?? 0 },
  ]

  switch (p.zones) {
    case 'blocked':
      return (
        <p>
          The zones come from the token.{' '}
          <button type="button" className="linkbtn" onClick={() => go('token')}>
            Add a token first
          </button>
          .
        </p>
      )
    case 'waiting':
      return (
        <>
          <p>The daemon reads the zones of the token in its next cycle.</p>
          <Busy label="Waiting for the first cycle with the token" />
          <LeftOut excluded={excluded} />
        </>
      )
    case 'empty':
      return (
        <>
          <p>
            A cycle ran with the token and found no zone. Under Zone Resources of the token include the zones you publish in, and add <b>Zone &gt; Zone &gt; Read</b>
            , then check the token again in step 1.
          </p>
          <LeftOut excluded={excluded} />
        </>
      )
  }
  return (
    <>
      <p>These are the zones as the daemon uses them. In the usual case there is nothing to do.</p>
      <Table label="Zones" columns={columns} rows={st.zones} rowKey={(z) => z.id || z.name} height={240} />
      {st.zones.some((z) => z.credentials.length > 1 && !z.pinned) && (
        <p>
          pco does not decide which credential serves a zone that two list: pin it on the zone&apos;s page. Until then the credential that served it before keeps serving it,
          and without one the account of the zone is left as it is.
        </p>
      )}
      <LeftOut excluded={excluded} />
      <p className="wizard-next">
        <Button variant="primary" onClick={() => go('route')}>
          Continue to the first route
        </Button>
      </p>
    </>
  )
}
