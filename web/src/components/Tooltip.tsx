import { cloneElement, type ReactElement, type ReactNode, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'

// The room between the child and the bubble, and the least between the
// bubble and the edges of the window, in pixels.
const gap = 6

// Tooltip shows content beside its child while the pointer is over either
// or the child has focus, and goes with Esc (WCAG 1.4.13). The content
// describes the child for a screen reader whether it is shown or not. The
// bubble is fixed to the window, so that no box that scrolls, such as a
// table's, cuts it off: above the child, or below it where there is no room.
export function Tooltip({ content, children }: { content: ReactNode; children: ReactElement<{ 'aria-describedby'?: string }> }) {
  const id = useId()
  const [shown, setShown] = useState(false)
  const anchor = useRef<HTMLSpanElement>(null)
  const bubble = useRef<HTMLSpanElement>(null)

  useEffect(() => {
    if (!shown) return
    const dismiss = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setShown(false)
    }
    document.addEventListener('keydown', dismiss)
    return () => document.removeEventListener('keydown', dismiss)
  }, [shown])

  useLayoutEffect(() => {
    const tip = bubble.current
    if (!shown || !tip) return
    const place = () => {
      const child = anchor.current?.getBoundingClientRect()
      if (!child) return
      const { width, height } = tip.getBoundingClientRect()
      const above = child.top - gap - height
      const left = Math.min(child.left + child.width / 2 - width / 2, window.innerWidth - width - gap)
      tip.style.top = `${above >= 0 ? above : child.bottom + gap}px`
      tip.style.left = `${Math.max(left, gap)}px`
    }
    place()
    // A scroll of any box around the child moves it.
    window.addEventListener('scroll', place, true)
    window.addEventListener('resize', place)
    return () => {
      window.removeEventListener('scroll', place, true)
      window.removeEventListener('resize', place)
    }
  }, [shown])

  return (
    <span
      ref={anchor}
      className="tip"
      onMouseEnter={() => setShown(true)}
      onMouseLeave={() => setShown(false)}
      onFocus={() => setShown(true)}
      onBlur={() => setShown(false)}
    >
      {cloneElement(children, { 'aria-describedby': id })}
      <span ref={bubble} role="tooltip" id={id} className="tip-bubble" hidden={!shown}>
        {content}
      </span>
    </span>
  )
}
