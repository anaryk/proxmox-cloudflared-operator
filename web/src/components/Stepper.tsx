import type { ReactNode } from 'react'

import { LevelBadge } from './StateBadge'

export interface Step {
  key: string
  name: ReactNode
  // ok, warn, fail, skipped, info; another level shows as unknown
  level: string
  // The word for the level when it is not the level itself, such as
  // "skipped: an earlier step failed".
  word?: ReactNode
  detail?: ReactNode
}

// Stepper shows the steps of a check one under the other, each with its
// level in an icon and a word. The detail of the first failure is open; the
// others are a click away.
export function Stepper({ steps, label }: { steps: readonly Step[]; label: string }) {
  const firstFailure = steps.findIndex((s) => s.level === 'fail')
  return (
    <ol className="stepper" aria-label={label}>
      {steps.map((step, at) => (
        <li key={step.key} className={`step step-${step.level}`}>
          <div className="step-head">
            <span className="step-name">{step.name}</span>
            <LevelBadge level={step.level}>{step.word}</LevelBadge>
          </div>
          {step.detail !== undefined &&
            (at === firstFailure ? (
              <div className="step-detail">{step.detail}</div>
            ) : (
              <details className="step-more">
                <summary>Details</summary>
                <div className="step-detail">{step.detail}</div>
              </details>
            ))}
        </li>
      ))}
    </ol>
  )
}
