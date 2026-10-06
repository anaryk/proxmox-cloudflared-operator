import { Badge } from '../../components/Badge'
import { Untrusted } from '../../components/Untrusted'
import { routeKey } from '../../text/routes'
import type { Change, PublishedRoute } from './diff'

const entries = (items: readonly string[], kind: 'ins' | 'del') =>
  items.map((item) => {
    const Mark = kind
    return (
      <li key={`${kind}${item}`}>
        <Mark>
          <span className="sr-only">{kind === 'ins' ? 'added ' : 'removed '}</span>
          <Untrusted text={item} />
        </Mark>
      </li>
    )
  })

const text = (value: string) => (value === 'none' ? <span className="muted">none</span> : <Untrusted text={value} />)

// Changes lists the settings that differ, each with what it was and what it
// becomes; a list shows the entries that came and went. The fields in both
// were changed on the other side as well.
export function Changes({ changes, label, both }: { changes: readonly Change[]; label: string; both?: ReadonlySet<string> }) {
  return (
    <table className="changes" aria-label={label}>
      <thead>
        <tr>
          <th scope="col">Setting</th>
          <th scope="col">Change</th>
        </tr>
      </thead>
      <tbody>
        {changes.map((c) => (
          <tr key={c.field}>
            <th scope="row" className="mono">
              {c.field}
              {both?.has(c.field) && (
                <>
                  {' '}
                  <Badge tone="warn">changed on both sides</Badge>
                </>
              )}
            </th>
            <td>
              {c.added && c.removed ? (
                <ul className="changes-list">
                  {entries(c.removed, 'del')}
                  {entries(c.added, 'ins')}
                </ul>
              ) : (
                <>
                  <del>{text(c.before)}</del> <span aria-hidden="true">→</span> <span className="sr-only">becomes </span>
                  <ins>{text(c.after)}</ins>
                </>
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// The routes of a list that are named one by one; the rest are counted.
const named = 10

// FirstAllowed says what the first entry of allowHosts does, and names the
// routes published now that stop.
export function FirstAllowed({ stopped }: { stopped: readonly PublishedRoute[] }) {
  const shown = stopped.slice(0, named)
  return (
    <div className="settings-optin">
      <p>
        allowHosts has no entry now, so every hostname a guest names may be published but the apex of a zone and a wildcard. With this entry only the hostnames a
        pattern of the list matches are published, from the next cycle.
      </p>
      {stopped.length === 0 ? (
        <p className="muted">Every route published now matches a pattern of the list.</p>
      ) : (
        <>
          <p>
            {stopped.length === 1
              ? '1 route published now matches no pattern of the list and stops being published'
              : `${stopped.length} routes published now match no pattern of the list and stop being published`}
            ; their records and rules go once the grace has passed:
          </p>
          <ul>
            {shown.map((r) => (
              <li key={routeKey(r)}>
                <Untrusted text={r.hostname} hostname /> <span className="muted">(<Untrusted text={r.owner} />)</span>
              </li>
            ))}
          </ul>
          {stopped.length > shown.length && <p className="muted">and {stopped.length - shown.length} more</p>}
        </>
      )}
    </div>
  )
}
