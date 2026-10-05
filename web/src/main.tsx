import './theme/tokens.css'
import './theme/base.css'

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { startPreferences } from './theme/theme'

const root = document.getElementById('root')
if (!root) {
  throw new Error('index.html has no element with the id root')
}

startPreferences()

createRoot(root).render(
  <StrictMode>
    <main>
      <h1>pco</h1>
      <p>The web interface is on its way. Until then, pco status on the node shows the state.</p>
    </main>
  </StrictMode>,
)
