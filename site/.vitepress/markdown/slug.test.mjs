import assert from 'node:assert/strict'
import { test } from 'node:test'

import { slugify } from './slug.mjs'

test('ids as GitHub makes them', () => {
  const cases = [
    // docs/security.md links to this heading from its own text.
    ["A connector that is not pco's", 'a-connector-that-is-not-pcos'],
    ['The pco egress commands', 'the-pco-egress-commands'],
    ['Observe-only until pco apply', 'observe-only-until-pco-apply'],
    ['Records in the way, and pco adopt', 'records-in-the-way-and-pco-adopt'],
    ['Nothing happens: observe-only', 'nothing-happens-observe-only'],
    ['identityMinimum', 'identityminimum'],
    ['8.4 or later', '84-or-later'],
    ['gate_tag and allowHosts', 'gate_tag-and-allowhosts'],
    ['Zone > DNS > Edit', 'zone--dns--edit'],
    ['Café “menu”', 'café-menu'],
  ]
  for (const [text, id] of cases) {
    assert.equal(slugify(text), id, text)
  }
})
