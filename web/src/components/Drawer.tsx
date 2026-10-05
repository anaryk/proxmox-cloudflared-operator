import { type ReactNode, useEffect, useId, useLayoutEffect, useRef } from 'react'

import { IconButton } from './Button'
import { CloseIcon } from './icons'

export interface DrawerProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  children: ReactNode
}

// Drawer shows the details of what was picked beside the page, which stays
// usable: it is not modal. Opening it moves focus to its heading; Esc in it
// closes it, and focus goes back to the row or node that opened it
// (spec-ui 10).
export function Drawer({ open, onClose, title, children }: DrawerProps) {
  const ref = useRef<HTMLElement>(null)
  const heading = useRef<HTMLHeadingElement>(null)
  const titleId = useId()

  useLayoutEffect(() => {
    if (!open) return
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
    heading.current?.focus()
    const drawer = ref.current
    return () => {
      // Focus is lost with the drawer, or still in it: it goes back.
      const now = document.activeElement
      const lost = now === null || now === document.body || (drawer?.contains(now) ?? false)
      if (lost && opener?.isConnected) opener.focus()
    }
  }, [open])

  useEffect(() => {
    const drawer = ref.current
    if (!open || !drawer) return
    const escape = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      onClose()
    }
    drawer.addEventListener('keydown', escape)
    return () => drawer.removeEventListener('keydown', escape)
  }, [open, onClose])

  if (!open) return null
  return (
    <aside ref={ref} className="drawer" aria-labelledby={titleId}>
      <div className="drawer-head">
        <h2 id={titleId} ref={heading} tabIndex={-1}>
          {title}
        </h2>
        <IconButton label="Close" icon={<CloseIcon />} onClick={onClose} />
      </div>
      <div className="drawer-body">{children}</div>
    </aside>
  )
}
