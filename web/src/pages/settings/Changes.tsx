import { Untrusted } from '../../components/Untrusted'
import type { Change } from './diff'

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
// becomes; a list shows the entries that came and went.
export function Changes({ changes, label }: { changes: readonly Change[]; label: string }) {
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
