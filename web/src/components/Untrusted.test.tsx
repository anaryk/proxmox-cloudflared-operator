import { render } from '@testing-library/react'
import { describe, expect, test } from 'vitest'

import { Untrusted } from './Untrusted'

describe('Untrusted', () => {
  test.each([
    ['a right-to-left override', 'invoice‮txt.exe', 'invoice⟨U+202E⟩txt.exe'],
    ['a zero-width space', 'www.exa​mple.com', 'www.exa⟨U+200B⟩mple.com'],
    ['a bell', 'ding\u0007', 'ding⟨U+0007⟩'],
  ])('%s becomes a marker', (_, text, shown) => {
    const { container } = render(<Untrusted text={text} />)
    expect(container.textContent).toBe(shown)
    expect(container.querySelectorAll('.cp')).toHaveLength(1)
  })

  test('ordinary text stays as it is, in one isolated run', () => {
    const { container } = render(<Untrusted text="služba 日本語: connection refused" />)
    const bdi = container.querySelector('bdi')
    expect(bdi?.textContent).toBe('služba 日本語: connection refused')
    expect(bdi?.getAttribute('dir')).toBeNull()
    expect(container.querySelector('.cp')).toBeNull()
  })

  test('markup is text', () => {
    const { container } = render(<Untrusted text={'<img src=x onerror="alert(1)">'} />)
    expect(container.querySelector('img')).toBeNull()
    expect(container.textContent).toBe('<img src=x onerror="alert(1)">')
  })

  test('a hostname reads left to right', () => {
    const { container } = render(<Untrusted text="www.example.com" hostname />)
    expect(container.querySelector('bdi')?.getAttribute('dir')).toBe('ltr')
  })

  test('a hostname in Punycode: the ASCII form, then the Unicode form, marked', () => {
    const { container } = render(<Untrusted text="xn--bcher-kva.example" hostname />)
    const forms = [...container.querySelectorAll('bdi')].map((b) => b.textContent)
    expect(forms).toEqual(['xn--bcher-kva.example', 'bücher.example'])
    expect(container.textContent).toContain('internationalised name')
  })

  test('a hostname in Punycode that does not decode stays ASCII', () => {
    const { container } = render(<Untrusted text="xn--bcher-k*a.example" hostname />)
    expect(container.textContent).toBe('xn--bcher-k*a.example')
  })

  test('the Unicode form of a hostname is marked as well', () => {
    // U+202E written in Punycode
    const { container } = render(<Untrusted text="xn--a-qin.example" hostname />)
    expect(container.textContent).toContain('a⟨U+202E⟩.example')
  })

  test('outside a hostname, Punycode is text', () => {
    const { container } = render(<Untrusted text="xn--bcher-kva.example" />)
    expect(container.textContent).toBe('xn--bcher-kva.example')
  })

  describe('max', () => {
    test('cuts with an ellipsis and keeps the whole text in the title', () => {
      const { container } = render(<Untrusted text={'abcdef‮ghij'} max={8} />)
      const bdi = container.querySelector('bdi')
      expect(bdi?.textContent).toBe('abcdef⟨U+202E⟩…')
      expect(bdi?.getAttribute('title')).toBe('abcdef⟨U+202E⟩ghij')
    })

    test('counts characters, not halves of them', () => {
      const { container } = render(<Untrusted text="🙂🙂🙂🙂" max={3} />)
      expect(container.querySelector('bdi')?.textContent).toBe('🙂🙂…')
    })

    test('leaves text that fits', () => {
      const { container } = render(<Untrusted text="short" max={5} />)
      const bdi = container.querySelector('bdi')
      expect(bdi?.textContent).toBe('short')
      expect(bdi?.hasAttribute('title')).toBe(false)
    })
  })
})
