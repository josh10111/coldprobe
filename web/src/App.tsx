import { BrowserRouter, Link, Route, Routes } from 'react-router-dom'
import { ActiveRunProvider } from './ActiveRun'
import { Layout } from './Layout'
import { ComparePage } from './pages/ComparePage'
import { Home } from './pages/Home'
import { NewRun } from './pages/NewRun'
import { RunPage } from './pages/RunPage'
import { ToastProvider } from './Toaster'

// Toasts render router links and the active run raises toasts, hence the nesting order.
export function App() {
  return (
    <BrowserRouter>
      <ToastProvider>
        <ActiveRunProvider>
          <Routes>
            <Route element={<Layout />}>
              <Route index element={<Home />} />
              <Route path="new" element={<NewRun />} />
              <Route path="runs/:id" element={<RunPage />} />
              <Route path="compare" element={<ComparePage />} />
              <Route
                path="*"
                element={
                  <p>
                    No such page. <Link to="/">Home</Link>
                  </p>
                }
              />
            </Route>
          </Routes>
        </ActiveRunProvider>
      </ToastProvider>
    </BrowserRouter>
  )
}
