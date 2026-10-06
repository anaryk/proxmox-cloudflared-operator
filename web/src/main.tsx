import './theme/tokens.css'
import './theme/base.css'

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { App } from './app/App'
import { startPreferences } from './theme/theme'

const root = document.getElementById('root')
if (!root) {
  throw new Error('index.html has no element with the id root')
}

startPreferences()

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
