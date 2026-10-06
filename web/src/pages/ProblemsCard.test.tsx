import { render, screen } from '@testing-library/react'
import { expect, test, vi } from 'vitest'

import type { State } from '../api/types.gen'
import first from '../fixtures/first-run.json'
import populated from '../fixtures/populated.json'
import { ProblemsCard } from './ProblemsCard'

test('before the first cycle: the problems the daemon started with, and no next step yet', () => {
  const { container } = render(<ProblemsCard state={first as unknown as State} />)
  expect(screen.getByRole('heading', { name: '1 problem' })).toBeTruthy()
  expect([...container.querySelectorAll('.problems li')].map((li) => li.textContent)).toEqual(['no Cloudflare credential; add one with pco credential add'])
  expect(container.querySelector('.next-step')).toBeNull()
  // the problems pill of the top bar leads here
  expect(container.querySelector('#problems')).toBeTruthy()
})

test('every line verbatim and in the daemon\'s order, the same line twice included, then the next step', () => {
  const error = vi.spyOn(console, 'error').mockImplementation(() => {})
  const st: State = { ...(populated as unknown as State), mode: 'observe', problems: ['b comes first', 'a second', 'a second'] }
  const { container } = render(<ProblemsCard state={st} />)
  expect(screen.getByRole('heading', { name: '3 problems' })).toBeTruthy()
  expect([...container.querySelectorAll('.problems li')].map((li) => li.textContent)).toEqual(['b comes first', 'a second', 'a second'])
  expect(container.querySelector('.next-step')?.textContent).toBe('Run pco apply to start publishing.')
  // React says so for two items of one key
  expect(error).not.toHaveBeenCalled()
  error.mockRestore()
})

test('a line is text, and what would turn it around shows as its code point', () => {
  const st: State = { ...(populated as unknown as State), problems: ['guest says \u202eevil<b>'] }
  const { container } = render(<ProblemsCard state={st} />)
  expect(container.querySelector('.problems b')).toBeNull()
  expect(container.querySelector('.problems .cp')?.textContent).toBe('⟨U+202E⟩')
})

test('no problems, no card', () => {
  const { container } = render(<ProblemsCard state={{ ...(populated as unknown as State), problems: [] }} />)
  expect(container.childElementCount).toBe(0)
})
