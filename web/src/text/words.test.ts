import { describe, expect, test } from 'vitest'

import cases from '../../../internal/present/testdata/words.json'
import { routeStateOrder } from '../gen/words.gen'
import { isBidi, printable } from './chars'
import {
  budgetWait,
  type CommandWords,
  commandArg,
  compareRouteStates,
  connectorText,
  credentialState,
  egressText,
  fixCommand,
  identityNow,
  inventoryText,
  isCommand,
  modeText,
  nextStep,
  rogueText,
  rotateCommand,
  routeNote,
  setupCommand,
  type TunnelView,
  unaffected,
  verifiedText,
  writerText,
} from './words'

interface Case {
  fn: string
  name: string
  input: unknown
  want: unknown
}

const composed = (c: CommandWords) => ({ command: c.command?.text ?? '', refused: c.refused ?? '' })

// As the Go test: each of the characters in place of each character of each
// value, and between any two, must be refused as of an unexpected form; each
// value must pass as it is. The list is what did not hold.
function commandArgEveryPosition(input: { chars: string[]; values: Record<string, string> }): string[] {
  const wrong: string[] = []
  for (const kind of Object.keys(input.values).sort()) {
    const v = input.values[kind] ?? ''
    if (commandArg(kind, v).value !== v) wrong.push(`${kind} ${JSON.stringify(v)} is refused`)
    for (const c of input.chars) {
      const spliced: string[] = []
      for (let i = 0; i <= v.length; i++) {
        if (i < v.length && v[i] !== c) spliced.push(v.slice(0, i) + c + v.slice(i + 1))
        spliced.push(v.slice(0, i) + c + v.slice(i))
      }
      for (const s of spliced) {
        const got = commandArg(kind, s)
        if (got.value !== '' || got.refused !== `the ${kind} has an unexpected form`) wrong.push(`${kind} ${JSON.stringify(s)}`)
      }
    }
  }
  return wrong
}

// The TypeScript twin of each function of internal/present that words.json
// has cases for. A case of a function without a twin fails.
const twins: Record<string, (input: never) => unknown> = {
  modeText,
  inventoryText,
  egressText,
  writerText,
  verifiedText,
  connectorText,
  rogueText,
  credentialState,
  identityNow,
  routeNote,
  nextStep,
  unaffected,
  routeStateOrder: () => routeStateOrder,
  commandArg: (input: { kind: string; value: string }) => commandArg(input.kind, input.value),
  commandArgEveryPosition,
  rotateCommand: (input: { account: string; tunnels: TunnelView[] }) => composed(rotateCommand(input.tunnels, input.account)),
  budgetWait,
  printable,
  isBidi: (input: string) => isBidi(input.codePointAt(0) ?? -1),
}

describe('the cases of internal/present/testdata/words.json', () => {
  const all = cases as Case[]

  test('there are cases', () => {
    expect(all.length).toBeGreaterThan(100)
  })

  test.each(all.map((c) => [`${c.fn}: ${c.name}`, c] as const))('%s', (_, c) => {
    const twin = Object.hasOwn(twins, c.fn) ? twins[c.fn] : undefined
    if (!twin) {
      throw new Error(`words.json has cases for ${c.fn}, which has no twin in words.ts`)
    }
    expect(twin(c.input as never)).toEqual(c.want ?? [])
  })
})

describe('commandArg', () => {
  test('a kind named like a property of every object has no form', () => {
    expect(commandArg('constructor', 'qemu/101')).toEqual({ value: '', refused: 'the constructor has an unexpected form' })
  })
})

describe('the words of a lookup keep a state they do not know', () => {
  test.each(['toString', 'constructor', '__proto__'])('%s', (word) => {
    expect(egressText({ state: word })).toBe(word)
    expect(writerText(word)).toBe(word)
  })
})

describe('rotateCommand', () => {
  const tunnel = {
    accountId: '0123456789abcdef0123456789abcdef',
    credentialId: 'a1b2c3d4',
    name: 'pco-abc123',
    id: '6f1d0c2e-2b8a-4c43-9a51-3e2d7b6c1a90',
    exists: true,
  }

  test('two tunnels in one account are refused as the daemon refuses them', () => {
    expect(rotateCommand([tunnel, { ...tunnel, id: 'other' }], tunnel.accountId)).toEqual({
      refused: `invalid request: the install has tunnels in accounts ${tunnel.accountId}; name one with --account`,
    })
  })

  test('a tunnel whose existence is not known is no target', () => {
    expect(rotateCommand([{ ...tunnel, unknown: true }], tunnel.accountId).refused).toMatch(/^not found: /)
  })
})

describe('fixCommand', () => {
  const account = '0123456789abcdef0123456789abcdef'

  test.each([
    'pco guest approve qemu/101',
    'pco guest revoke lxc/200',
    `pco tunnel rotate --account ${account}`,
    'pco setup --recover',
    'pco egress on',
    'pco apply',
    'pco diagnose www.example.com',
    // the id pco gives a credential
    'pco credential check a1b2c3d4',
  ])('%s is a command', (fix) => {
    const got = fixCommand(fix)
    expect(isCommand(got.command)).toBe(true)
    expect(got.command?.text).toBe(fix)
  })

  test.each([
    ['advice that is no pco command', 'journalctl -u pco says why it does not come'],
    ['advice that begins with a command', 'wait for the first cycle; journalctl -u pco says why it does not come'],
    ['a command and prose', 'pco status lists the problems that say why'],
    ['two commands in a sentence', 'pco status says why; pco egress show shows the table'],
    ['a second command after the value', 'pco guest approve qemu/1; reboot'],
    ['a second command after a flag', 'pco guest approve qemu/1 && reboot'],
    ['a line break', 'pco guest approve qemu/1\nreboot'],
    ['a substitution', 'pco guest approve $(reboot)'],
    ['a backtick', 'pco guest approve `reboot`'],
    ['a placeholder', 'pco credential add --label <label>'],
    ['a flag with its value attached', `pco tunnel rotate --account=${account}`],
    ['a value of no known form', 'pco credential check cred1'],
    ['a credential id in capitals', 'pco credential check A1B2C3D4'],
    ['an owner of a manual route', 'pco guest approve manual/shop'],
    ['a value that is an option', 'pco guest approve -qemu/101'],
    ['a flag that ends the options', 'pco guest approve -- qemu/101'],
    ['a short flag', 'pco guest approve -f qemu/101'],
    ['three words before the arguments', 'pco route manual add shop.example.com'],
    ['a word after a value', 'pco guest approve qemu/101 now'],
    ['no subcommand', 'pco'],
    ['a flag in place of the subcommand', 'pco --version'],
    ['a space too many', 'pco  guest approve qemu/101'],
    ['a space at the end', 'pco guest approve qemu/101 '],
    ['a space at the start', ' pco guest approve qemu/101'],
    ['a tab', 'pco\tguest approve qemu/101'],
    ['a prefix', 'sudo pco guest approve qemu/101'],
    ['nothing', ''],
  ])('%s is text: %s', (_name, fix) => {
    const got = fixCommand(fix)
    expect(got.command).toBeUndefined()
    expect(got.refused).not.toBe('')
  })

  // Characters that look like those of a command, and are not.
  test.each([
    ['a no-break space between the words', 'pco\u00a0guest approve qemu/101'],
    ['a no-break space before the value', 'pco guest approve\u00a0qemu/101'],
    ['a narrow no-break space', 'pco guest approve\u202fqemu/101'],
    ['an ideographic space', 'pco guest\u3000approve qemu/101'],
    ['a cyrillic p in pco', '\u0440co guest approve qemu/101'],
    ['a cyrillic o in the subcommand', 'pco guest appr\u043eve qemu/101'],
    ['a cyrillic a in a hostname', 'pco diagnose www.ex\u0430mple.com'],
    ['a greek o in an owner', 'pco guest approve qem\u03bf/101'],
    ['a full-width letter in the subcommand', 'pco \uff47uest approve qemu/101'],
    ['a full-width digit in an owner', 'pco guest approve qemu/10\uff11'],
    ['an arabic-indic digit in a credential id', 'pco credential check a1b2c3d\u0664'],
    ['a full-width solidus in an owner', 'pco guest approve qemu\uff0f101'],
    ['a fraction slash in an owner', 'pco guest approve qemu\u2044101'],
    ['a dash that is no hyphen-minus in a flag', 'pco tunnel rotate \u2010\u2010account 0123456789abcdef0123456789abcdef'],
    ['a zero-width joiner in a word', 'pco gu\u200dest approve qemu/101'],
    ['a soft hyphen in a hostname', 'pco diagnose www.exam\u00adple.com'],
  ])('%s is text', (_name, fix) => {
    expect(fixCommand(fix).command).toBeUndefined()
  })
})

describe('Command', () => {
  const made = setupCommand.command

  test('is what this module made, and nothing that looks like it', () => {
    expect(made?.text).toBe('pco setup')
    expect(isCommand(made)).toBe(true)
    expect(isCommand('pco setup')).toBe(false)
    expect(isCommand({ text: 'pco setup' })).toBe(false)
    expect(isCommand(Object.create(Object.getPrototypeOf(made)))).toBe(false)
  })

  test('cannot be made elsewhere, not even with its constructor', () => {
    const Made = Object.getPrototypeOf(made).constructor
    expect(() => new Made(Symbol('command'), 'reboot')).toThrow(TypeError)
  })
})

describe('budgetWait', () => {
  test.each([
    ['a count with a plus sign', "+2 changes wait for Cloudflare's rate limit"],
    ['a count with a leading zero', "02 changes wait for Cloudflare's rate limit"],
    ['a negative count', "-2 changes wait for Cloudflare's rate limit"],
    ['reads without the and', "the listing of zone a.example, the listing of zone b.example wait for Cloudflare's rate limit"],
  ])('%s is not the line', (_, line) => {
    expect(budgetWait(line)).toEqual({ changes: 0, matched: false })
  })

  test('three reads and changes', () => {
    const line =
      "the listing of zone a.example, the listing of zone b.example, the tunnel of account 0123456789abcdef0123456789abcdef and 12 changes wait for Cloudflare's rate limit"
    expect(budgetWait(line)).toEqual({ changes: 12, matched: true })
  })
})

describe('compareRouteStates', () => {
  test('the listed states in their order, then the others by name', () => {
    const states = ['zeta', 'frozen', 'active', 'alpha', 'rejected', 'held', 'no-zone']
    expect([...states].sort(compareRouteStates)).toEqual(['active', 'no-zone', 'held', 'rejected', 'frozen', 'alpha', 'zeta'])
  })
})
