import type { AnchorHTMLAttributes, MouseEvent } from 'react'

import { navigate } from './router'

// Link goes to a view of the page without loading it again; with a key held
// or another button, the browser does what it does with a link.
export function Link({ to, onClick, children, ...rest }: AnchorHTMLAttributes<HTMLAnchorElement> & { to: string }) {
  const click = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e)
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    e.preventDefault()
    navigate(to)
  }
  return (
    <a {...rest} href={to} onClick={click}>
      {children}
    </a>
  )
}
