import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import App from './App'
import { DialogProvider } from './components/dialogs'
import { StepUpGate } from './components/StepUpGate'
import './index.css'

const qc = new QueryClient({
  defaultOptions: { queries: { retry: 1, staleTime: 10_000, refetchOnWindowFocus: false } },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={qc}>
      <BrowserRouter>
        <DialogProvider>
          <App />
          <StepUpGate />
        </DialogProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
