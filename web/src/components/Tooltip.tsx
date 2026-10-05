import { cloneElement, type ReactElement, type ReactNode, useEffect, useId, useState } from 'react'

// Tooltip shows content beside its child while the pointer is over either
// or the child has focus, and goes with Esc (WCAG 1.4.13). The content
// describes the child for a screen reader whether it is shown or not.
export function Tooltip({ content, children }: { content: ReactNode; children: ReactElement<{ 'aria-describedby'?: string }> }) {
  const id = useId()
  const [shown, setShown] = useState(false)
  useEffect(() => {
    if (!shown) return
    const dismiss = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setShown(false)
    }
    document.addEventListener('keydown', dismiss)
    return () => document.removeEventListener('keydown', dismiss)
  }, [shown])
  return (
    <span
      className="tip"
      onMouseEnter={() => setShown(true)}
      onMouseLeave={() => setShown(false)}
      onFocus={() => setShown(true)}
      onBlur={() => setShown(false)}
    >
      {cloneElement(children, { 'aria-describedby': id })}
      <span role="tooltip" id={id} className="tip-bubble" hidden={!shown}>
        {content}
      </span>
    </span>
  )
}
