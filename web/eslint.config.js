import js from '@eslint/js'
import { defineConfig, globalIgnores } from 'eslint/config'
import jsxA11y from 'eslint-plugin-jsx-a11y'
import reactHooks from 'eslint-plugin-react-hooks'
import globals from 'globals'
import tseslint from 'typescript-eslint'

const sink = 'Text from guests and from Cloudflare must never reach an HTML sink: render it as text.'
const code = 'The Content-Security-Policy refuses to run strings as code.'

export default defineConfig([
  globalIgnores(['test-results/', 'playwright-report/']),
  js.configs.recommended,
  {
    files: ['**/*.{ts,tsx}'],
    extends: [tseslint.configs.strict, reactHooks.configs.flat.recommended, jsxA11y.flatConfigs.recommended],
    languageOptions: { globals: globals.browser },
  },
  {
    files: ['**/*.{js,mjs}', 'vite.config.ts'],
    languageOptions: { globals: globals.node },
  },
  {
    rules: {
      'no-implied-eval': 'error',
      'no-restricted-syntax': [
        'error',
        { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: sink },
        { selector: "Property[key.name='dangerouslySetInnerHTML']", message: sink },
        { selector: "CallExpression[callee.name='eval']", message: code },
        { selector: "CallExpression[callee.name='Function']", message: code },
        { selector: "NewExpression[callee.name='Function']", message: code },
      ],
      'no-restricted-properties': [
        'error',
        { property: 'innerHTML', message: sink },
        { property: 'outerHTML', message: sink },
        { property: 'insertAdjacentHTML', message: sink },
        { object: 'document', property: 'write', message: sink },
        { object: 'document', property: 'writeln', message: sink },
        { property: 'eval', message: code },
        { property: 'Function', message: code },
      ],
    },
  },
])
