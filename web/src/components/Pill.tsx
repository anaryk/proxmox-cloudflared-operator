import type { ReactNode } from 'react'

import { type Tone, ToneIcon } from './icons'

export interface PillProps {
  tone: Tone
  label: string
  value: ReactNode
  // A pill that opens something is a button.
  onClick?: () => void
  expanded?: boolean
  controls?: string
}

// Pill is one status of the top bar: its icon, what it is and its value.
export function Pill({ tone, label, value, onClick, expanded, controls }: PillProps) {
  const content = (
    <>
      <ToneIcon tone={tone} />
      <span className="pill-label">{label}</span>
      <b className="pill-value">{value}</b>
    </>
  )
  if (!onClick) return <span className={`pill pill-${tone}`}>{content}</span>
  return (
    <button type="button" className={`pill pill-${tone}`} onClick={onClick} aria-expanded={expanded} aria-controls={controls}>
      {content}
    </button>
  )
}
