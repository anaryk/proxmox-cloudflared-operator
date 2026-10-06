// Fails when a package the interface ships at run time has a licence outside
// the list in allowed. It reads package-lock.json and needs nothing installed:
// every package npm does not mark as for development only counts. The build
// holds what it bundles to the same list (vite.config.ts).

import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

export const allowed = ['MIT', 'ISC', 'BSD-2-Clause', 'BSD-3-Clause', 'Apache-2.0', '0BSD', 'CC0-1.0']

const known = new Set(allowed.map((id) => id.toLowerCase()))

// licenceAllowed reads the SPDX expressions packages carry: one licence, or
// licences joined by OR and AND, with parentheses. A choice is allowed when
// one side is, AND when both are. Anything else, a missing licence, WITH an
// exception or "SEE LICENSE IN" among them, is not.
export function licenceAllowed(expression) {
  if (typeof expression !== 'string') {
    return false
  }
  const tokens = expression.match(/[()]|[^\s()]+/g) ?? []
  let at = 0

  const either = () => {
    let ok = both()
    while (tokens[at]?.toUpperCase() === 'OR') {
      at++
      ok = both() || ok
    }
    return ok
  }
  const both = () => {
    let ok = one()
    while (tokens[at]?.toUpperCase() === 'AND') {
      at++
      ok = one() && ok
    }
    return ok
  }
  const one = () => {
    const token = tokens[at++]
    if (token === '(') {
      const ok = either()
      if (tokens[at++] !== ')') {
        throw new SyntaxError('unclosed parenthesis')
      }
      return ok
    }
    if (token === undefined || token === ')' || /^(?:OR|AND|WITH)$/i.test(token)) {
      throw new SyntaxError(`unexpected ${token ?? 'end'}`)
    }
    return known.has(token.toLowerCase())
  }

  try {
    const ok = either()
    return ok && at === tokens.length
  } catch {
    return false
  }
}

// runtimePackages lists the packages of a lock file (version 2 or 3) that are
// installed for run time, the project itself left out.
export function runtimePackages(lock) {
  if (!lock.packages) {
    throw new Error(`package-lock.json of version ${lock.lockfileVersion} has no packages; npm 7 or later writes them`)
  }
  return Object.entries(lock.packages)
    .filter(([path, entry]) => path !== '' && !entry.dev && !entry.link)
    .map(([path, entry]) => ({
      name: path.slice(path.lastIndexOf('node_modules/') + 'node_modules/'.length),
      version: entry.version,
      license: entry.license,
    }))
}

function main() {
  const lock = JSON.parse(readFileSync(new URL('../package-lock.json', import.meta.url), 'utf8'))
  const packages = runtimePackages(lock)
  const refused = packages.filter((pkg) => !licenceAllowed(pkg.license))
  for (const pkg of refused) {
    console.error(`${pkg.name}@${pkg.version} is under ${pkg.license ?? 'no licence it names'}, which is not one of ${allowed.join(', ')}`)
  }
  if (refused.length > 0) {
    process.exit(1)
  }
  console.log(`${packages.length} packages for run time, each under ${allowed.join(', ')} or a choice among them`)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main()
}
