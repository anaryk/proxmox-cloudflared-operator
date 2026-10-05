import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { ToastProvider, useToast } from './Toast'

function Actions() {
  const toast = useToast()
  return (
    <>
      <button type="button" onClick={() => toast('Saved at revision 4.', 'ok')}>
        Save
      </button>
      <button type="button" onClick={() => toast('No answer in time: the outcome is unknown.', 'fail')}>
        Apply
      </button>
    </>
  )
}

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

const regions = (container: HTMLElement) => ({
  polite: container.querySelector('[aria-live="polite"]'),
  assertive: container.querySelector('[aria-live="assertive"]'),
})

test('a result is announced politely and goes by itself', () => {
  const { container } = render(
    <ToastProvider>
      <Actions />
    </ToastProvider>,
  )
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  expect(regions(container).polite?.textContent).toBe('Saved at revision 4.')
  act(() => {
    vi.advanceTimersByTime(6000)
  })
  expect(regions(container).polite?.textContent).toBe('')
})

test('a failure is announced at once and stays until dismissed', () => {
  const { container } = render(
    <ToastProvider>
      <Actions />
    </ToastProvider>,
  )
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }))
  act(() => {
    vi.advanceTimersByTime(60_000)
  })
  expect(regions(container).assertive?.textContent).toBe('No answer in time: the outcome is unknown.')
  fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }))
  expect(regions(container).assertive?.textContent).toBe('')
})

test('useToast outside of the provider is a mistake of the page', () => {
  vi.spyOn(console, 'error').mockImplementation(() => {})
  expect(() => render(<Actions />)).toThrow(/outside of a ToastProvider/)
})
