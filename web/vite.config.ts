import { fileURLToPath } from 'node:url'

import react from '@vitejs/plugin-react'
import license from 'rollup-plugin-license'
import { defineConfig } from 'vite'

import { licenceAllowed } from './scripts/check-licences.mjs'

// pco web embeds this directory (internal/web/ui, build tag webui) and gzips
// it itself at start, so nothing here is compressed.
const dist = fileURLToPath(new URL('../internal/web/ui/dist/', import.meta.url))

export default defineConfig({
  plugins: [
    react(),
    license({
      thirdParty: {
        includePrivate: false,
        // What is bundled must pass the same list as what package-lock.json
        // installs for run time: a package listed for development only and
        // imported anyway is caught here.
        allow: {
          test: (dependency) => licenceAllowed(dependency.license),
          failOnUnlicensed: true,
          failOnViolation: true,
        },
        output: { file: `${dist}licenses.txt` },
      },
    }),
  ],
  build: {
    outDir: dist,
    emptyOutDir: true,
    assetsDir: 'assets',
    target: ['chrome120', 'edge120', 'firefox115', 'safari17'],
    cssCodeSplit: true,
    modulePreload: { polyfill: false },
    sourcemap: false,
  },
})
