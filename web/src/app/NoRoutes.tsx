import type { State } from '../api/types.gen'
import { Empty } from '../components/Empty'
import { Untrusted } from '../components/Untrusted'
import { Link } from './Link'

const annotationsDoc = 'https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/docs/annotations.md'

const snippet = '```cf-tunnel\napp.example.com -> :3000\n```'

const zeroTime = (at?: string) => !at || at.startsWith('0001-01-01T00:00:00')

function Notes() {
  return (
    <>
      <p>Its routes go into the Notes of the guest, one per line:</p>
      <pre className="snippet">
        <code>{snippet}</code>
      </pre>
    </>
  )
}

// NoRoutes is what the Overview and the Routes page show without a route
// (spec-ui 7.1). No routes is two states, told apart by how many guests carry
// the gate tag; before the first cycle it is neither.
export function NoRoutes({ state, gateTag }: { state: State; gateTag?: string }) {
  const tag = <span className="mono">{gateTag ? <Untrusted text={gateTag} /> : 'of the settings'}</span>
  if (zeroTime(state.at)) {
    return (
      <Empty title="Waiting for the first cycle">
        {state.problems.length > 0 && (
          <ul className="problems">
            {state.problems.map((p) => (
              <li key={p}>
                <Untrusted text={p} />
              </li>
            ))}
          </ul>
        )}
      </Empty>
    )
  }
  const approve =
    state.admission === 'approve' ? <p>Admission is set to approve: the routes of a new guest wait until an admin approves the guest.</p> : null
  const n = state.gateTagged
  if (n === 0) {
    return (
      <Empty title={`No guest carries the tag ${gateTag ?? 'of the settings'}`}>
        <p>Give a guest the tag {tag} in Proxmox VE, on the Summary page of the guest, and pco reads its Notes.</p>
        <Notes />
        {approve}
        <p>
          <a href={annotationsDoc}>How to write routes in the Notes</a>
        </p>
      </Empty>
    )
  }
  return (
    <Empty title={`${n === 1 ? '1 guest carries' : `${n} guests carry`} the tag ${gateTag ?? 'of the settings'}, but none names a hostname in its Notes`}>
      <Notes />
      {approve}
      <p>
        <Link to="/guests">The guests</Link> say which carry the tag, and what pco found in their Notes.
      </p>
    </Empty>
  )
}
