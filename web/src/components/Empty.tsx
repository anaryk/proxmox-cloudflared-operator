import type { ReactNode } from 'react'

import { InfoIcon } from './icons'

// Empty says why there is nothing to show and what to do about it; never a
// blank table.
export function Empty({ title, children, action, icon }: { title: string; children?: ReactNode; action?: ReactNode; icon?: ReactNode }) {
  return (
    <div className="empty">
      <span className="empty-icon">{icon ?? <InfoIcon />}</span>
      <p className="empty-title">{title}</p>
      {children && <div className="empty-text">{children}</div>}
      {action && <div className="empty-action">{action}</div>}
    </div>
  )
}
