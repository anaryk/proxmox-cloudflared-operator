import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

const root = document.getElementById('root')
if (!root) {
  throw new Error('index.html has no element with the id root')
}

createRoot(root).render(
  <StrictMode>
    <main>
      <h1>pco</h1>
      <p>The web interface is on its way. Until then, pco status on the node shows the state.</p>
    </main>
  </StrictMode>,
)
