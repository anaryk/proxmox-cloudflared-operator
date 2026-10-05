import type { ReactNode } from 'react'

import { ToneIcon } from './icons'

// Banner says something about the whole installation under the top bar,
// with at most one action. The shell orders them, most severe first.
export function Banner({ tone, children, action }: { tone: 'fail' | 'warn' | 'info'; children: ReactNode; action?: ReactNode }) {
  return (
    <div className={`banner banner-${tone}`}>
      <ToneIcon tone={tone} />
      <div className="banner-text">{children}</div>
      {action && <div className="banner-action">{action}</div>}
    </div>
  )
}
