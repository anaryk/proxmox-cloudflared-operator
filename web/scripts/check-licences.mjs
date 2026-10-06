// Fails when a package the interface ships at run time has a licence outside
// the list in allowed, or a package of the documentation site one outside the
// list of the site in lists. It reads web/package-lock.json, or the lock file
// it is given, and needs nothing installed: every package npm does not mark as
// for development only counts. The build holds what it bundles to the same
// list (vite.config.ts).

import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

export const allowed = ['MIT', 'ISC', 'BSD-2-Clause', 'BSD-3-Clause', 'Apache-2.0', '0BSD', 'CC0-1.0']

// The lists, by the name of the project whose lock file is read. The
// documentation site in site/ is published on its own and is no part of the
// package. Mermaid, which draws the diagrams of its pages, brings
// robust-predicates under the Unlicense and elkjs under EPL-2.0, which takes
// the permissive side where a release offers EPL-2.0 OR GPL, and khroma,
// whose package.json names no licence and whose licence file is MIT; a named
// licence holds for that version only. Vite minifies the stylesheet with
// lightningcss, under MPL-2.0, whose code does not reach the site.
export const lists = {
  'pco-web': { allowed },
  'pco-site': {
    allowed: [...allowed, 'Unlicense', 'EPL-2.0', 'MPL-2.0'],
    named: { 'khroma@2.1.0': 'MIT' },
  },
}

// licenceAllowed reads the SPDX expressions packages carry: one licence, or
// licences joined by OR and AND, with parentheses. A choice is allowed when
// one side is, AND when both are. Anything else, a missing licence, WITH an
// exception or "SEE LICENSE IN" among them, is not.
export function licenceAllowed(expression, list = allowed) {
  if (typeof expression !== 'string') {
    return false
  }
  const known = new Set(list.map((id) => id.toLowerCase()))
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

// refusedPackages returns the packages for run time of a lock file whose
// licence is not on the list of its project, and that list.
export function refusedPackages(lock) {
  const project = lock.name ?? lock.packages?.['']?.name
  const list = lists[project]
  if (!list) {
    throw new Error(`there is no list of licences for the project ${project}`)
  }
  const packages = runtimePackages(lock).map((pkg) => ({
    ...pkg,
    license: list.named?.[`${pkg.name}@${pkg.version}`] ?? pkg.license,
  }))
  return { packages, refused: packages.filter((pkg) => !licenceAllowed(pkg.license, list.allowed)), list }
}

// The lock file of the web interface, or the one named.
function main() {
  const path = process.argv[2] ?? fileURLToPath(new URL('../package-lock.json', import.meta.url))
  const { packages, refused, list } = refusedPackages(JSON.parse(readFileSync(path, 'utf8')))
  for (const pkg of refused) {
    console.error(`${pkg.name}@${pkg.version} is under ${pkg.license ?? 'no licence it names'}, which is not one of ${list.allowed.join(', ')}`)
  }
  if (refused.length > 0) {
    process.exit(1)
  }
  console.log(`${packages.length} packages for run time, each under ${list.allowed.join(', ')} or a choice among them`)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main()
}
