import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { type Command, type CommandWords, egressOnCommand, rotateCommand } from '../text/words'
import { CopyCommand } from './CopyCommand'

afterEach(() => {
  vi.restoreAllMocks()
})

const account = '0123456789abcdef0123456789abcdef'
const tunnel = { accountId: account, credentialId: 'a1b2c3d4', name: 'pco-abc123', id: '6f1d0c2e', exists: true }

describe('CopyCommand', () => {
  test('copies the command as it is shown', async () => {
    const write = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    render(<CopyCommand cmd={rotateCommand([tunnel], account)} />)
    expect(screen.getByText(`pco tunnel rotate --account ${account}`).tagName).toBe('CODE')
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Copy' }))
    })
    expect(write).toHaveBeenCalledWith(`pco tunnel rotate --account ${account}`)
    expect(screen.getByRole('status').textContent).toBe('Copied.')
  })

  test('says where to run it when asked', () => {
    render(<CopyCommand cmd={egressOnCommand} root />)
    expect(screen.getByText('pco egress on')).toBeTruthy()
    expect(screen.getByText(/Run it as root on the node\./)).toBeTruthy()
  })

  test('says so when the browser refuses to copy', async () => {
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new DOMException('denied', 'NotAllowedError'))
    render(<CopyCommand cmd={egressOnCommand} />)
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Copy' }))
    })
    expect(screen.getByRole('status').textContent).toMatch(/^The browser did not let the page copy it/)
  })

  test('a refusal is no command and nothing to copy', () => {
    const { container } = render(<CopyCommand cmd={rotateCommand([tunnel], 'abc; rm -rf /')} />)
    expect(container.textContent).toBe('No command to copy: the account id has an unexpected form')
    expect(screen.queryByRole('button')).toBeNull()
    expect(container.querySelector('code')).toBeNull()
  })

  test('the reason of a refusal is shown as untrusted text', () => {
    const held = { ...tunnel, leftAsIs: true, held: 'zone ex‮ample.com is frozen' }
    const { container } = render(<CopyCommand cmd={rotateCommand([held], account)} />)
    expect(container.textContent).toContain('zone ex⟨U+202E⟩ample.com is frozen')
  })

  // The first check is the compiler's: npm run typecheck fails when a plain
  // string is taken for a command. The second is CopyCommand's own.
  test('a string the page puts together is no command', () => {
    const owner = 'qemu/101; reboot'
    // @ts-expect-error only words.ts makes a Command
    const made: CommandWords = { command: `pco guest approve ${owner}` }
    expect(made.command).toBe('pco guest approve qemu/101; reboot')
    const { container } = render(<CopyCommand cmd={made} />)
    expect(container.textContent).toBe('No command to copy: it was not composed from checked values')
    expect(screen.queryByRole('button')).toBeNull()
  })

  test('nor is an object cast to one', () => {
    const made = { command: { text: 'pco guest approve qemu/101; reboot' } as unknown as Command }
    const { container } = render(<CopyCommand cmd={made} />)
    expect(container.textContent).toBe('No command to copy: it was not composed from checked values')
    expect(container.querySelector('code')).toBeNull()
  })
})
