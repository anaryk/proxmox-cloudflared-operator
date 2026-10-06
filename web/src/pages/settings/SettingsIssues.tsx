import type { Issue } from '../../api/types.gen'
import { WarnIcon } from '../../components/icons'
import { Untrusted } from '../../components/Untrusted'

// settingsLines are the issues of the settings, those no guest has, as
// pco status prints them, and the daemon's notes on settings it uses
// otherwise than stored.
export function settingsLines(issues: readonly Issue[], notes: readonly string[]): string[] {
  const lines = issues.filter((i) => !i.guest).map((i) => `settings: ${i.msg}`)
  return [...lines, ...notes.filter((n) => !lines.includes(n))]
}

export function SettingsIssues({ issues, notes = [] }: { issues: readonly Issue[]; notes?: readonly string[] }) {
  const lines = settingsLines(issues, notes)
  if (lines.length === 0) return null
  return (
    <section className="card" aria-labelledby="settings-issues">
      <div className="card-head">
        <WarnIcon />
        <h2 id="settings-issues">{lines.length === 1 ? '1 issue with the settings' : `${lines.length} issues with the settings`}</h2>
      </div>
      <div className="card-body">
        <ul className="problems">
          {lines.map((line) => (
            <li key={line}>
              <Untrusted text={line} />
            </li>
          ))}
        </ul>
      </div>
    </section>
  )
}
