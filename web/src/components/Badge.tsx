import type { ReactNode } from 'react'

import type { Tone } from './icons'

// Badge is a short label on the weak colour of its tone: a tag, a count, a
// marker such as "wildcard". outline draws the plain one the level of a
// route uses ("port", "filtered").
export function Badge({ tone, outline, children }: { tone?: Tone; outline?: boolean; children: ReactNode }) {
  const classes = ['badge', tone && `badge-${tone}`, outline && 'badge-outline'].filter(Boolean).join(' ')
  return <span className={classes}>{children}</span>
}
