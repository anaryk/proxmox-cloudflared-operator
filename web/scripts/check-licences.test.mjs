import { describe, expect, test } from 'vitest'

import { licenceAllowed, runtimePackages } from './check-licences.mjs'

describe('licenceAllowed', () => {
  test.each([
    ['MIT', true],
    ['ISC', true],
    ['0BSD', true],
    ['CC0-1.0', true],
    ['mit', true],
    ['(MIT OR Apache-2.0)', true],
    ['MIT OR GPL-3.0-only', true],
    ['(GPL-3.0-only OR MIT)', true],
    ['MIT AND ISC', true],
    ['(MIT AND (BSD-3-Clause OR GPL-2.0-only))', true],
    ['GPL-3.0-only', false],
    ['MIT AND GPL-3.0-only', false],
    ['(MIT AND (BSD-3-Clause OR GPL-2.0-only)) AND LGPL-2.1-only', false],
    ['Apache-2.0 WITH LLVM-exception', false],
    ['SEE LICENSE IN LICENSE.md', false],
    ['UNLICENSED', false],
    ['MIT OR', false],
    ['(MIT', false],
    ['MIT)', false],
    ['', false],
    [undefined, false],
  ])('%s', (expression, ok) => {
    expect(licenceAllowed(expression)).toBe(ok)
  })
})

describe('runtimePackages', () => {
  test('leaves out the project and what is for development only', () => {
    const lock = {
      lockfileVersion: 3,
      packages: {
        '': { name: 'pco-web', license: 'MIT' },
        'node_modules/react': { version: '19.3.0', license: 'MIT' },
        'node_modules/vite': { version: '8.3.2', license: 'MIT', dev: true },
        'node_modules/a/node_modules/b': { version: '1.0.0', license: 'GPL-3.0-only', peer: true },
        'node_modules/c': { version: '1.0.0', optional: true },
        'node_modules/d': { version: '1.0.0', license: 'MIT', devOptional: true },
      },
    }
    expect(runtimePackages(lock)).toEqual([
      { name: 'react', version: '19.3.0', license: 'MIT' },
      { name: 'b', version: '1.0.0', license: 'GPL-3.0-only' },
      { name: 'c', version: '1.0.0', license: undefined },
      { name: 'd', version: '1.0.0', license: 'MIT' },
    ])
  })

  test('refuses a lock file without packages', () => {
    expect(() => runtimePackages({ lockfileVersion: 1, dependencies: {} })).toThrow(/npm 7 or later/)
  })
})
