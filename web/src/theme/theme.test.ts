import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { preferences, setDensity, setTheme, startPreferences } from './theme'

const root = document.documentElement

beforeEach(() => {
  localStorage.clear()
  delete root.dataset.theme
  delete root.dataset.density
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('what is stored', () => {
  test('a theme and the compact density', () => {
    localStorage.setItem('pco.theme', 'dark')
    localStorage.setItem('pco.density', 'compact')
    startPreferences()
    expect(preferences()).toEqual({ theme: 'dark', density: 'compact' })
    expect(root.dataset.theme).toBe('dark')
    expect(root.dataset.density).toBe('compact')
  })

  test('nothing is the system and comfortable', () => {
    startPreferences()
    expect(preferences()).toEqual({ theme: 'system', density: 'comfortable' })
    expect(root.dataset.theme).toBeUndefined()
    expect(root.dataset.density).toBeUndefined()
  })

  test('a value of another version is the system', () => {
    localStorage.setItem('pco.theme', 'blue')
    localStorage.setItem('pco.density', 'roomy')
    startPreferences()
    expect(preferences()).toEqual({ theme: 'system', density: 'comfortable' })
  })
})

describe('a choice', () => {
  test('is stored and applied', () => {
    setTheme('light')
    setDensity('compact')
    expect(localStorage.getItem('pco.theme')).toBe('light')
    expect(localStorage.getItem('pco.density')).toBe('compact')
    expect(root.dataset.theme).toBe('light')
  })

  test('of the system and comfortable removes the keys', () => {
    setTheme('dark')
    setDensity('compact')
    setTheme('system')
    setDensity('comfortable')
    expect(localStorage.getItem('pco.theme')).toBeNull()
    expect(localStorage.getItem('pco.density')).toBeNull()
    expect(root.dataset.theme).toBeUndefined()
    expect(root.dataset.density).toBeUndefined()
  })
})

describe('when the browser refuses storage', () => {
  test.each([
    [
      'the property throws',
      () =>
        vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => {
          throw new DOMException('The operation is insecure.', 'SecurityError')
        }),
    ],
    [
      'reading and writing throw',
      () => {
        const refuse = () => {
          throw new DOMException('The quota has been exceeded.', 'QuotaExceededError')
        }
        vi.spyOn(Storage.prototype, 'getItem').mockImplementation(refuse)
        vi.spyOn(Storage.prototype, 'setItem').mockImplementation(refuse)
        vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(refuse)
      },
    ],
  ])('%s: the system theme, and a choice holds for the page', (_, refuse) => {
    refuse()
    expect(() => startPreferences()).not.toThrow()
    expect(preferences()).toEqual({ theme: 'system', density: 'comfortable' })
    expect(() => setTheme('dark')).not.toThrow()
    expect(preferences().theme).toBe('dark')
    expect(root.dataset.theme).toBe('dark')
  })
})
