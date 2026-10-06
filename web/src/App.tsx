import { Routes, Route, Navigate, useLocation } from 'react-router'
import { useQuery } from '@tanstack/react-query'
import { api, type User, ApiError } from './api'
import Login from './pages/Login'
import Invite from './pages/Invite'
import Shell from './pages/Shell'
import Chat from './pages/Chat'
import Settings from './pages/Settings'
import Admin from './pages/Admin'
import Jobs from './pages/Jobs'
import Training from './pages/Training'
import Media from './pages/Media'
import Agents from './pages/Agents'
import AgentMonitor from './pages/Agent'

export function useMe() {
  return useQuery<User | null>({
    queryKey: ['me'],
    queryFn: async () => {
      try {
        return await api.get<User>('/api/me')
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) return null
        throw e
      }
    },
  })
}

function RequireAuth({ children }: { children: React.ReactNode }) {
  const me = useMe()
  const loc = useLocation()
  if (me.isLoading) return <Centered>Loading…</Centered>
  if (!me.data) return <Navigate to="/login" state={{ from: loc.pathname }} replace />
  return <>{children}</>
}

/** Owner-only pages: a member who types the URL sees why, not an empty console. */
function RequireOwner({ children }: { children: React.ReactNode }) {
  const me = useMe()
  if (me.isLoading) return <Centered>Loading…</Centered>
  if (me.data?.role !== 'owner') return <Centered>This page is for the workspace owner.</Centered>
  return <>{children}</>
}

export function Centered({ children }: { children: React.ReactNode }) {
  return <div className="h-full grid place-items-center tagline text-fg-3">{children}</div>
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/invite/:token" element={<Invite />} />
      <Route
        path="/*"
        element={
          <RequireAuth>
            <Shell />
          </RequireAuth>
        }
      >
        <Route index element={<Centered>Pick a conversation or start a new one.</Centered>} />
        <Route path="c/:id" element={<Chat />} />
        <Route path="settings" element={<Settings />} />
        <Route path="admin" element={<RequireOwner><Admin /></RequireOwner>} />
        <Route path="jobs" element={<RequireOwner><Jobs /></RequireOwner>} />
        <Route path="training" element={<RequireOwner><Training /></RequireOwner>} />
        <Route path="media" element={<Media />} />
        <Route path="images" element={<Navigate to="/media" replace />} />
        <Route path="agents" element={<Agents />} />
        <Route path="agents/:id" element={<AgentMonitor />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}
