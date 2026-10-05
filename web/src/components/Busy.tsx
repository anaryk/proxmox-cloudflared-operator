import { useEffect, useState } from 'react'

export function elapsedText(ms: number): string {
  const s = Math.max(Math.floor(ms / 1000), 0)
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${s % 60} s`
}

// Busy says that something runs, and for how long: the daemon reports no
// progress, so there is no percentage and no bar that fills (spec-ui 4.2).
// since is when it began, in milliseconds of Date.now(), for an indicator
// that is shown again after a reload of its part of the page.
export function Busy({ label, since }: { label: string; since?: number }) {
  const [start] = useState(() => since ?? Date.now())
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const elapsed = now - start
  return (
    <span className="busy">
      <span className="busy-track" aria-hidden="true">
        <span className="busy-bar" />
      </span>
      <span role="status">{label}</span>
      <span className="busy-time num">{elapsedText(elapsed)}</span>
    </span>
  )
}
