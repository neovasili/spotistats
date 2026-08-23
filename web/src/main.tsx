import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import './theme.css'
import './charts/charts.css'
import './app.css'
import './explorer/explorer.css'
import './artist/artist.css'

const root = document.getElementById('root')
if (!root) throw new Error('no #root element')

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
