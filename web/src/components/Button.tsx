import { type ButtonHTMLAttributes, type MouseEvent, type ReactNode, useId } from 'react'

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'default' | 'primary' | 'danger'
  small?: boolean
  icon?: ReactNode
  // Why the action cannot be taken now, said next to the button. The button
  // stays focusable, so the reason is read with it.
  disabledReason?: string
}

export function Button({ variant = 'default', small, icon, disabledReason, className, children, onClick, type = 'button', ...rest }: ButtonProps) {
  const reason = useId()
  const classes = ['btn', variant !== 'default' && `btn-${variant}`, small && 'btn-small', className].filter(Boolean).join(' ')
  const refused = disabledReason !== undefined
  const click = (e: MouseEvent<HTMLButtonElement>) => {
    if (refused) e.preventDefault()
    else onClick?.(e)
  }
  const button = (
    <button
      {...rest}
      type={refused ? 'button' : type}
      className={classes}
      aria-disabled={refused || undefined}
      aria-describedby={refused ? reason : rest['aria-describedby']}
      onClick={click}
    >
      {icon}
      {children}
    </button>
  )
  if (!refused) return button
  return (
    <span className="btn-refused">
      {button}
      <span id={reason} className="btn-reason">
        {disabledReason}
      </span>
    </span>
  )
}

export interface IconButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'children'> {
  label: string
  icon: ReactNode
}

export function IconButton({ label, icon, className, type = 'button', ...rest }: IconButtonProps) {
  return (
    <button {...rest} type={type} className={className ? `iconbtn ${className}` : 'iconbtn'} aria-label={label} title={label}>
      {icon}
    </button>
  )
}
