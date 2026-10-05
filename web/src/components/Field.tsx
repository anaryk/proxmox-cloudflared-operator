import { type ReactNode, useId } from 'react'

import { FailIcon } from './icons'

// What a control needs to be tied to its label, hint and error.
export interface ControlProps {
  id: string
  'aria-describedby'?: string
  'aria-invalid'?: true
}

// Field is a labelled control with its hint and its error, both tied to it
// with aria-describedby (spec-ui 10). children draws the control with the
// props it is given.
export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: ReactNode
  hint?: ReactNode
  error?: ReactNode
  children: (control: ControlProps) => ReactNode
}) {
  const id = useId()
  const hintId = `${id}hint`
  const errorId = `${id}error`
  const describedBy = [hint !== undefined && hintId, error !== undefined && errorId].filter(Boolean).join(' ')
  return (
    <div className={error !== undefined ? 'field field-invalid' : 'field'}>
      <label htmlFor={id}>{label}</label>
      {children({ id, 'aria-describedby': describedBy || undefined, 'aria-invalid': error !== undefined ? true : undefined })}
      {hint !== undefined && (
        <p id={hintId} className="field-hint">
          {hint}
        </p>
      )}
      {error !== undefined && (
        <p id={errorId} className="field-error">
          <FailIcon />
          <span>{error}</span>
        </p>
      )}
    </div>
  )
}
