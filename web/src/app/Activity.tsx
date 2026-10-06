import { useEffect, useState } from 'react'

import { useApp, useStore } from '../api/store'
import { Button } from '../components/Button'
import { Dialog } from '../components/Dialog'
import { durationText } from '../text/duration'
import { useNow } from './clock'

// Input in the page counts as activity, as a call the user makes does: the
// page tells the web process at most once a minute (spec-ui 8.2), so that
// someone reading a long list is not signed out.
export const touchEvery = 60_000
const inputs = ['keydown', 'pointerdown', 'wheel'] as const

export function followInput(target: EventTarget, touch: () => void, now: () => number = Date.now): () => void {
  let last = -Infinity
  const seen = () => {
    const t = now()
    if (t - last < touchEvery) return
    last = t
    touch()
  }
  for (const type of inputs) target.addEventListener(type, seen, { passive: true, capture: true })
  return () => {
    for (const type of inputs) target.removeEventListener(type, seen, { capture: true })
  }
}

// The warning comes this long before the session idles out.
export const warnBefore = 2 * 60_000

// Activity follows the input of the page and warns two minutes before the
// session idles out, with "Stay signed in", which tells the web process at
// once.
export function Activity() {
  const store = useStore()
  const session = useApp((s) => s.session)
  const now = useNow()
  // the deadline this tab last asked the web process about
  const [asked, setAsked] = useState<number>()

  useEffect(() => followInput(document, () => void store.touch()), [store])

  const deadline = session ? store.idleDeadline() : undefined
  const due = deadline !== undefined && now >= deadline - warnBefore && now < deadline
  useEffect(() => {
    // Another tab may have kept the session alive: ask before warning.
    if (!due || deadline === asked) return
    let current = true
    void store.refreshSession().then(() => {
      if (current) setAsked(deadline)
    })
    return () => {
      current = false
    }
  }, [due, deadline, asked, store])

  const open = due && asked === deadline
  return (
    <Dialog
      open={open}
      onClose={() => void store.touch()}
      title="Your session is about to end"
      footer={
        <Button variant="primary" onClick={() => void store.touch()}>
          Stay signed in
        </Button>
      }
    >
      <p>Nothing was done in pco for a while: the session ends in {deadline === undefined ? '' : durationText(deadline - now)}, and you will have to sign in again.</p>
    </Dialog>
  )
}
