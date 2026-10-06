import { type ReactNode, type RefObject, useEffect, useId, useLayoutEffect, useRef } from 'react'

import { IconButton } from './Button'
import { CloseIcon } from './icons'

export interface DialogProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  children: ReactNode
  footer?: ReactNode
}

function giveBack(opener: RefObject<HTMLElement | null>): void {
  const el = opener.current
  opener.current = null
  if (el?.isConnected) el.focus()
}

// Dialog is the browser's own modal dialog: showModal() keeps focus in it
// and makes the page behind inert. Esc closes it, and focus goes back to
// what opened it.
export function Dialog({ open, onClose, title, children, footer }: DialogProps) {
  const ref = useRef<HTMLDialogElement>(null)
  const opener = useRef<HTMLElement | null>(null)
  const titleId = useId()

  useLayoutEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (open && !dialog.open) {
      opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
      dialog.showModal()
    } else if (!open && dialog.open) {
      dialog.close()
    }
  }, [open])

  // Browsers close a modal dialog on Esc by themselves; this does it where
  // the DOM does not, as the one the tests run in.
  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    const escape = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      dialog.close()
    }
    dialog.addEventListener('keydown', escape)
    return () => dialog.removeEventListener('keydown', escape)
  }, [])

  // A dialog taken off the page while open gives focus back as well.
  useEffect(() => () => giveBack(opener), [])

  return (
    <dialog
      ref={ref}
      className="dialog"
      aria-labelledby={titleId}
      onClose={() => {
        giveBack(opener)
        if (open) onClose()
      }}
    >
      {open && (
        <>
          <div className="dialog-head">
            <h2 id={titleId}>{title}</h2>
            <IconButton label="Close" icon={<CloseIcon />} onClick={() => ref.current?.close()} />
          </div>
          <div className="dialog-body">{children}</div>
          {footer && <div className="dialog-foot">{footer}</div>}
        </>
      )}
    </dialog>
  )
}
