import { fileURLToPath } from 'node:url'

import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The page of the performance test, built as the interface is (React for
// production, the same targets) into test-results, which git and the linter
// leave alone. playwright.config.ts builds and serves it.
export default defineConfig({
  root: fileURLToPath(new URL('.', import.meta.url)),
  plugins: [react()],
  build: {
    outDir: fileURLToPath(new URL('../../test-results/perf-page/', import.meta.url)),
    emptyOutDir: true,
    target: ['chrome120', 'edge120', 'firefox115', 'safari17'],
  },
})
