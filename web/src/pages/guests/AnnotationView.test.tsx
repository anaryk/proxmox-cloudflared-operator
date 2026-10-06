import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import type { AnnotationView as Annotation } from '../../api/types.gen'
import annotation from '../../fixtures/annotation.json'
import { AnnotationView, caret } from './AnnotationView'

const view = annotation as Annotation

// The rows of the block as they read, the number of the line in the Notes
// first.
function rows(container: HTMLElement): string[] {
  return [...(container.querySelector('pre')?.children ?? [])].map((row) => row.textContent ?? '')
}

test('the block with the lines of the Notes, and the issue under its line and column', () => {
  const { container } = render(<AnnotationView view={view} />)
  // the issue is at line 5 of the Notes: the second line of the block,
  // which begins at line 4; column 5 is under the first "e" of "example"
  expect(rows(container)).toEqual(['4```cf-tunnel', '5app.example.com -> :3000', '    ^', 'line 5, column 5: a broken entry', '6```'])
  const line = container.querySelectorAll('.annotation-line')[1]
  const caretRow = line?.nextElementSibling
  expect(caretRow?.className).toBe('annotation-caret')
  expect(caretRow?.getAttribute('aria-hidden')).toBe('true')
  expect(caretRow?.lastChild?.textContent).toBe('    ^')
  expect('app.example.com -> :3000'[caret(5).length - 1]).toBe('e')
  expect(caretRow?.nextElementSibling?.className).toBe('annotation-msg')
})

test('the block is a region named for what it is; the pre carries no name of its own', () => {
  const { container } = render(<AnnotationView view={view} />)
  const region = screen.getByRole('region', { name: `The route text of the Notes of ${view.ref}, from line 4` })
  expect(region.querySelector('pre')).toBe(container.querySelector('pre'))
  expect(container.querySelector('pre')?.hasAttribute('aria-label')).toBe(false)
})

test('the caret of a column', () => {
  expect(caret(1)).toBe('^')
  expect(caret(5)).toBe('    ^')
  expect(caret(undefined)).toBe('^')
  expect(caret(0)).toBe('^')
})

test('the text is the guest’s: a control character shows as its code point', () => {
  const { container } = render(<AnnotationView view={{ ...view, block: 'app.example.com -> :3000‮', startLine: 5 }} />)
  expect(container.querySelector('.cp')?.textContent).toMatch(/202E/)
})

test('an issue outside the block is listed under it', () => {
  const { container } = render(<AnnotationView view={{ ...view, issues: [...view.issues, { guest: { kind: 'qemu', vmid: 103 }, msg: 'too many hostnames' }] }} />)
  expect(container.querySelector('ul')?.textContent).toBe('too many hostnames')
})

test('a guest without route text', () => {
  const { container } = render(<AnnotationView view={{ ref: 'qemu/103', block: '', startLine: 0, issues: [] }} />)
  expect(container.textContent).toBe('The Notes of this guest have no route text.')
})
