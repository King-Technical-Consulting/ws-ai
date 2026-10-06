import { useState } from 'react'
import { Outlet, NavLink, useNavigate, Link } from 'react-router'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Project, type Conversation } from '../api'
import { useMe } from '../App'
import { WsMark, btn, inputSm } from '../components/ui'
import { Plus, Settings as SettingsIcon, Shield, LogOut, MessageSquare, Code2, SquareTerminal, Image as ImageIcon, Bot, Pencil, Archive, Palette, FlaskConical } from 'lucide-react'
import clsx from 'clsx'

/** "https://github.com/a/repo.git" -> "repo" */
function repoName(url: string): string {
  const m = url.trim().match(/\/([^/]+?)(?:\.git)?\/?$/)
  return m ? m[1] : ''
}

const navItem = ({ isActive }: { isActive: boolean }) =>
  clsx('flex items-center gap-2 rounded-md px-2 py-1.5 text-sm truncate w-full text-left', isActive ? 'bg-bg-3 text-fg' : 'text-fg-2 hover:bg-bg-3 hover:text-fg')

export default function Shell() {
  const me = useMe()
  const nav = useNavigate()
  const qc = useQueryClient()
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })
  const convs = useQuery({ queryKey: ['convs'], queryFn: () => api.get<Conversation[]>('/api/conversations/recent') })

  const newChat = useMutation({
    mutationFn: async () => {
      let p = projects.data?.[0]
      if (!p) {
        p = await api.post<Project>('/api/projects', { name: 'General', kind: 'chat' })
        await qc.invalidateQueries({ queryKey: ['projects'] })
      }
      return api.post<Conversation>(`/api/projects/${p.id}/conversations`, {})
    },
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ['convs'] })
      nav(`/c/${c.id}`)
    },
  })

  const [showCode, setShowCode] = useState(false)
  const [codeName, setCodeName] = useState('')
  const [repoUrl, setRepoUrl] = useState('')
  const newCode = useMutation({
    mutationFn: async () => {
      const p = await api.post<Project>('/api/projects', {
        name: codeName.trim() || repoName(repoUrl) || 'Code',
        kind: 'code',
        repo_url: repoUrl.trim() || undefined,
      })
      await qc.invalidateQueries({ queryKey: ['projects'] })
      return api.post<Conversation>(`/api/projects/${p.id}/conversations`, {})
    },
    onSuccess: (c) => {
      setShowCode(false)
      setCodeName('')
      setRepoUrl('')
      qc.invalidateQueries({ queryKey: ['convs'] })
      nav(`/c/${c.id}`)
    },
  })

  // A design project: conversations in design mode, where the assistant
  // makes design artifacts under the conversation's design system.
  const newDesign = useMutation({
    mutationFn: async () => {
      const name = prompt('Design project name', 'Design')
      if (name === null) throw new Error('cancelled')
      const p = await api.post<Project>('/api/projects', { name: name.trim() || 'Design', kind: 'design' })
      await qc.invalidateQueries({ queryKey: ['projects'] })
      return api.post<Conversation>(`/api/projects/${p.id}/conversations`, {})
    },
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ['convs'] })
      nav(`/c/${c.id}`)
    },
  })

  const rename = useMutation({
    mutationFn: ({ id, title }: { id: string; title: string }) => api.patch(`/api/conversations/${id}`, { title }),
    onSuccess: (_r, v) => {
      qc.invalidateQueries({ queryKey: ['convs'] })
      qc.invalidateQueries({ queryKey: ['conv', v.id] })
    },
  })
  const archive = useMutation({
    mutationFn: (id: string) => api.del(`/api/conversations/${id}`),
    onSuccess: (_r, id) => {
      qc.invalidateQueries({ queryKey: ['convs'] })
      if (location.pathname === `/c/${id}`) nav('/')
    },
  })

  async function logout() {
    await api.post('/api/auth/logout')
    await qc.invalidateQueries({ queryKey: ['me'] })
    nav('/login')
  }

  return (
    <div className="h-full flex">
      <aside className="w-64 shrink-0 border-r border-line bg-bg-2 flex flex-col">
        <div className="px-3 pt-3 pb-2 flex items-center justify-between">
          <Link to="/" className="wordmark inline-flex items-center gap-2">
            <WsMark size={22} className="text-fg" />
            ws
          </Link>
          <button
            onClick={() => newChat.mutate()}
            disabled={newChat.isPending}
            className="p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg disabled:opacity-60"
            title="New conversation"
            aria-label="New conversation"
          >
            <Plus size={18} />
          </button>
        </div>
        <div className="px-3 pb-2">
          {showCode ? (
            <form
              className="space-y-1.5 rounded-md border border-line bg-bg p-2"
              onSubmit={(e) => { e.preventDefault(); newCode.mutate() }}
            >
              <input value={repoUrl} onChange={(e) => setRepoUrl(e.target.value)} placeholder="https://github.com/you/repo (optional)" className={`${inputSm} w-full`} autoFocus />
              <input value={codeName} onChange={(e) => setCodeName(e.target.value)} placeholder="Project name" className={`${inputSm} w-full`} />
              <div className="flex gap-1.5">
                <button type="submit" disabled={newCode.isPending} className={btn.primarySm}>Create</button>
                <button type="button" onClick={() => setShowCode(false)} className={btn.secondarySm}>Cancel</button>
              </div>
              {newCode.error && <p className="text-xs text-danger">{(newCode.error as Error).message}</p>}
              <p className="meta">A code project gets a sandbox with the repo cloned; the assistant can read, edit and run code there.</p>
            </form>
          ) : (
            <button onClick={() => setShowCode(true)} className={clsx(navItem({ isActive: false }), 'text-xs')}>
              <Code2 size={14} /> New code project
            </button>
          )}
          <button onClick={() => newDesign.mutate()} disabled={newDesign.isPending} className={clsx(navItem({ isActive: false }), 'text-xs')}>
            <Palette size={14} /> New design project
          </button>
        </div>
        <div className="px-3">
          <h2 className="section-head">conversations</h2>
        </div>
        <nav className="flex-1 overflow-y-auto px-2 pt-2 space-y-0.5">
          {convs.data?.map((c) => (
            <div key={c.id} className="group relative">
              <NavLink to={`/c/${c.id}`} className={navItem}>
                <MessageSquare size={14} className="shrink-0" />
                <span className="truncate pr-10">{c.title || 'New conversation'}</span>
              </NavLink>
              <span className="absolute right-1 top-1/2 -translate-y-1/2 hidden group-hover:flex items-center gap-0.5">
                <button
                  onClick={() => {
                    const t = prompt('Rename conversation', c.title || '')
                    if (t !== null && t.trim()) rename.mutate({ id: c.id, title: t.trim() })
                  }}
                  className="p-1 rounded text-fg-3 hover:text-fg hover:bg-bg"
                  title="Rename"
                  aria-label="Rename conversation"
                >
                  <Pencil size={12} />
                </button>
                <button
                  onClick={() => {
                    if (confirm(`Archive "${c.title || 'New conversation'}"?`)) archive.mutate(c.id)
                  }}
                  className="p-1 rounded text-fg-3 hover:text-fg hover:bg-bg"
                  title="Archive"
                  aria-label="Archive conversation"
                >
                  <Archive size={12} />
                </button>
              </span>
            </div>
          ))}
          {convs.data?.length === 0 && <p className="meta px-2 py-4">No conversations yet.</p>}
        </nav>
        <div className="p-2 border-t border-line text-sm space-y-0.5">
          <NavLink to="/media" className={navItem}>
            <ImageIcon size={14} /> Media
          </NavLink>
          <NavLink to="/agents" className={navItem}>
            <Bot size={14} /> Agents
          </NavLink>
          <NavLink to="/settings" className={navItem}>
            <SettingsIcon size={14} /> Settings
          </NavLink>
          {me.data?.role === 'owner' && (
            <>
              <NavLink to="/jobs" className={navItem}>
                <SquareTerminal size={14} /> Jobs
              </NavLink>
              <NavLink to="/training" className={navItem}>
                <FlaskConical size={14} /> Training
              </NavLink>
              <NavLink to="/admin" className={navItem}>
                <Shield size={14} /> Admin
              </NavLink>
            </>
          )}
          <button onClick={logout} className={navItem({ isActive: false })}>
            <LogOut size={14} /> Sign out
          </button>
          <p className="px-2 pt-1 text-xs text-fg-4 truncate">{me.data?.email}</p>
        </div>
      </aside>
      <main className="flex-1 min-w-0 flex flex-col">
        <Outlet />
      </main>
    </div>
  )
}
