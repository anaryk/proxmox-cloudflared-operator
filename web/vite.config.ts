/// <reference types="vitest/config" />
import { fileURLToPath } from 'node:url'

import react from '@vitejs/plugin-react'
import license from 'rollup-plugin-license'
import { defineConfig } from 'vite'

import { licenceAllowed } from './scripts/check-licences.mjs'
import { mockApi } from './src/api/mock.ts'

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
    // VITE_MOCK=1 npm run dev: the development server answers /api from the
    // fixtures, VITE_MOCK_STATE names the state (src/api/mock.ts).
    process.env.VITE_MOCK === '1' && mockApi(process.env.VITE_MOCK_STATE),
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
  // The tests of the scripts run in Node, those of the interface in a DOM.
  test: {
    projects: [
      { extends: true, test: { name: 'scripts', include: ['*.test.mjs', 'scripts/**/*.test.mjs'], environment: 'node' } },
      {
        extends: true,
        test: {
          name: 'src',
          include: ['src/**/*.test.{ts,tsx}'],
          environment: 'happy-dom',
          setupFiles: ['src/test/setup.ts'],
          // contrast.test.ts reads the tokens as text.
          css: { include: [/\.css\?raw$/] },
        },
      },
    ],
  },
})
