import { expect, test } from 'vitest'

import { docsUrl } from './docs'

const at = (ref: string) => `https://github.com/anaryk/proxmox-cloudflared-operator/blob/${ref}/docs/annotations.md`

test.each([
  ['0.3.0', at('v0.3.0')],
  ['v1.3.0', at('v1.3.0')],
  ['v1.4.0-rc.1', at('v1.4.0-rc.1')],
  ['v1.3.0-5-g1a2b3c4', at('main')],
  ['v1.3.0-5-g1a2b3c4-dirty', at('main')],
  ['e860c64', at('main')],
  ['dev', at('main')],
  [undefined, at('main')],
])('version %s', (version, url) => {
  expect(docsUrl('annotations.md', version)).toBe(url)
})
