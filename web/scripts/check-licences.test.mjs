import { describe, expect, test } from 'vitest'

import { licenceAllowed, refusedPackages, runtimePackages } from './check-licences.mjs'

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

describe('refusedPackages', () => {
  const lock = (name, packages) => ({ name, lockfileVersion: 3, packages: { '': { name }, ...packages } })
  const sitePackages = {
    'node_modules/robust-predicates': { version: '3.0.3', license: 'Unlicense' },
    'node_modules/elkjs': { version: '0.9.3', license: 'EPL-2.0' },
    'node_modules/khroma': { version: '2.1.0' },
    'node_modules/lightningcss': { version: '1.33.0', license: 'MPL-2.0' },
    'node_modules/vue': { version: '3.5.0', license: 'MIT' },
  }

  test('holds the web interface to its list', () => {
    const { refused } = refusedPackages(lock('pco-web', sitePackages))
    expect(refused.map((pkg) => pkg.name)).toEqual(['robust-predicates', 'elkjs', 'khroma', 'lightningcss'])
  })

  test('allows the site what Mermaid and Vite bring', () => {
    const { packages, refused } = refusedPackages(lock('pco-site', sitePackages))
    expect(refused).toEqual([])
    expect(packages.find((pkg) => pkg.name === 'khroma')?.license).toBe('MIT')
  })

  test('takes the permissive side of a choice on the site', () => {
    const elkjs = { 'node_modules/elkjs': { version: '0.11.0', license: 'EPL-2.0 OR GPL-3.0-or-later' } }
    expect(refusedPackages(lock('pco-site', elkjs)).refused).toEqual([])
  })

  test('names a licence for one version only', () => {
    const { refused } = refusedPackages(lock('pco-site', { 'node_modules/khroma': { version: '2.2.0' } }))
    expect(refused.map((pkg) => `${pkg.name}@${pkg.version}`)).toEqual(['khroma@2.2.0'])
  })

  test('refuses a project it has no list for', () => {
    expect(() => refusedPackages(lock('other', {}))).toThrow(/no list of licences for the project other/)
  })
})
