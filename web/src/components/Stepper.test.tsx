import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { Stepper } from './Stepper'

const steps = [
  { key: 'route', name: 'route', level: 'ok', detail: 'qemu/101 holds it' },
  { key: 'zone', name: 'zone', level: 'warn', detail: 'the zone is checked again within 15 minutes' },
  { key: 'dns', name: 'dns', level: 'fail', detail: 'the record points elsewhere' },
  { key: 'tcp', name: 'tcp', level: 'fail', detail: 'connection refused' },
  { key: 'http', name: 'http', level: 'skipped', word: 'skipped: an earlier step failed' },
]

test('each step in order with its level in a word', () => {
  render(<Stepper label="Diagnosis of www.example.com" steps={steps} />)
  const list = screen.getByRole('list', { name: 'Diagnosis of www.example.com' })
  const items = [...list.querySelectorAll(':scope > li')]
  expect(items.map((li) => li.querySelector('.status-word')?.textContent)).toEqual(['ok', 'warn', 'fail', 'fail', 'skipped: an earlier step failed'])
})

test('the detail of the first failure is open, the others behind a click', () => {
  const { container } = render(<Stepper label="Diagnosis" steps={steps} />)
  const items = [...container.querySelectorAll('li')]
  expect(items[2]?.querySelector('details')).toBeNull()
  expect(items[2]?.querySelector('.step-detail')?.textContent).toBe('the record points elsewhere')
  expect(items[3]?.querySelector('details')?.open).toBe(false)
  expect(items[0]?.querySelector('details')?.open).toBe(false)
  expect(items[4]?.querySelector('.step-detail')).toBeNull()
})
