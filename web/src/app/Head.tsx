import type { ReactNode } from 'react'

// Head is the top of a page: its title, a line on what it is, and the actions
// that belong to the whole page.
export function Head({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="page-head">
      <div>
        <h1>{title}</h1>
        {description && <p className="page-description">{description}</p>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  )
}
