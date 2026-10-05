import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { Field } from './Field'

test('the label, the hint and the error are tied to the control', () => {
  render(
    <Field label="Gate tag" hint="Guests with this tag may ask for hostnames." error="A tag has letters, digits, - and _ only.">
      {(control) => <input {...control} />}
    </Field>,
  )
  const input = screen.getByRole('textbox', { name: 'Gate tag' })
  expect(input.getAttribute('aria-invalid')).toBe('true')
  const described = (input.getAttribute('aria-describedby') ?? '').split(' ').map((id) => document.getElementById(id)?.textContent)
  expect(described).toEqual(['Guests with this tag may ask for hostnames.', 'A tag has letters, digits, - and _ only.'])
})

test('without hint or error the control describes nothing', () => {
  render(<Field label="Label">{(control) => <input {...control} />}</Field>)
  const input = screen.getByRole('textbox', { name: 'Label' })
  expect(input.hasAttribute('aria-describedby')).toBe(false)
  expect(input.hasAttribute('aria-invalid')).toBe(false)
})
