import { ESLint } from 'eslint'
import { describe, expect, test } from 'vitest'

const eslint = new ESLint({ cwd: import.meta.dirname })

async function rules(code, filePath) {
  const [result] = await eslint.lintText(code, { filePath })
  return result.messages.map((m) => m.ruleId)
}

describe('sinks and code from strings are refused', () => {
  test.each([
    ['dangerouslySetInnerHTML', 'export const A = (p: { s: string }) => <div dangerouslySetInnerHTML={{ __html: p.s }} />'],
    ['dangerouslySetInnerHTML in props', "import { createElement } from 'react'\nexport const a = (s: string) => createElement('div', { dangerouslySetInnerHTML: { __html: s } })"],
    ['innerHTML', 'export const f = (e: Element, s: string) => { e.innerHTML = s }'],
    ['outerHTML', 'export const f = (e: Element, s: string) => { e.outerHTML = s }'],
    ['insertAdjacentHTML', "export const f = (e: Element, s: string) => { e.insertAdjacentHTML('beforeend', s) }"],
    ['document.write', 'export const f = (s: string) => { document.write(s) }'],
    ['eval', 'export const f = (s: string) => eval(s)'],
    ['window.eval', 'export const f = (s: string) => window.eval(s)'],
    ['new Function', 'export const f = (s: string) => new Function(s)'],
    ['Function', 'export const f = (s: string) => Function(s)'],
    ['globalThis.Function', 'export const f = (s: string) => globalThis.Function(s)'],
  ])('%s', async (_, code) => {
    expect(await rules(code, 'src/sink.tsx')).toEqual(
      expect.arrayContaining([expect.stringMatching(/^no-restricted-(?:syntax|properties)$/)]),
    )
  })

  test('in scripts too', async () => {
    expect(await rules('export const f = (e, s) => { e.innerHTML = s }\n', 'scripts/sink.mjs')).toContain('no-restricted-properties')
  })

  test('text is fine', async () => {
    expect(await rules('export const A = (p: { s: string }) => <p>{p.s}</p>\n', 'src/text.tsx')).toEqual([])
  })
})
