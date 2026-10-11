import { useEffect, useRef, useState } from 'react'
import { Outlet, NavLink, useNavigate, useLocation, Link } from 'react-router'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Project, type Conversation } from '../api'
import { useMe } from '../App'
import { WsMark, btn, inputSm, useConfirm, usePrompt } from '../components/ui'
import { LocalityBadge } from '../components/LocalityBadge'
import { BookOpen, CalendarClock, Star, Plus, Settings as SettingsIcon, Shield, LogOut, MessageSquare, Code2, SquareTerminal, Image as ImageIcon, Bot, Pencil, Archive, Palette, FlaskConical, Menu, X } from 'lucide-react'
import clsx from 'clsx'

/** "https://github.com/a/repo.git" -> "repo" */
function repoName(url: string): string {
  const m = url.trim().match(/\/([^/]+?)(?:\.git)?\/?$/)
  return m ? m[1] : ''
}

const navItem = ({ isActive }: { isActive: boolean }) =>
  clsx('flex items-center gap-2 rounded-md px-2 py-2.5 md:py-1.5 text-sm truncate w-full text-left', isActive ? 'bg-bg-3 text-fg' : 'text-fg-2 hover:bg-bg-3 hover:text-fg')

// An inline title editor: Enter saves, Escape cancels, leaving the field saves. An empty title
// is not a title, so it saves nothing.
function RenameField({ value, onChange, onSave, onCancel }: { value: string; onChange: (v: string) => void; onSave: (v: string) => void; onCancel: () => void }) {
  const done = useRef(false)
  const finish = (save: boolean) => {
    if (done.current) return
    done.current = true
    if (save) onSave(value.trim())
    else onCancel()
  }
  return (
    <input
      value={value}
      onChange={(e) => onChange(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === 'Enter') { e.preventDefault(); finish(true) }
        else if (e.key === 'Escape') { e.preventDefault(); finish(false) }
      }}
      onBlur={() => finish(true)}
      onFocus={(e) => e.currentTarget.select()}
      aria-label="Conversation name"
      maxLength={200}
      className={`${inputSm} min-h-9 md:min-h-0`}
      autoFocus
    />
  )
}

// The less used pages and Sign out, in one popover above a button at the bottom of the sidebar.
// Opens on click, closes on Escape (focus goes back to the button), on a click outside, on
// leaving it with Tab, and on navigation.
function SidebarMenu({ owner, who, email, onSignOut }: { owner: boolean; who?: string; email?: string; onSignOut: () => void }) {
  const [open, setOpen] = useState(false)
  const box = useRef<HTMLDivElement>(null)
  const button = useRef<HTMLButtonElement>(null)
  const loc = useLocation()
  useEffect(() => setOpen(false), [loc.pathname])
  useEffect(() => {
    if (!open) return
    box.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus()
    const away = (e: MouseEvent) => { if (!box.current?.contains(e.target as Node)) setOpen(false) }
    // Tabbing out of the menu closes it. focusin, not blur: Safari gives a clicked button no focus,
    // so a blur handler would close the menu before the click lands.
    const left = (e: FocusEvent) => { if (!box.current?.contains(e.target as Node)) setOpen(false) }
    document.addEventListener('mousedown', away)
    document.addEventListener('focusin', left)
    return () => { document.removeEventListener('mousedown', away); document.removeEventListener('focusin', left) }
  }, [open])
  const item = (isActive: boolean) => clsx(navItem({ isActive }), 'min-h-9')
  return (
    <div
      ref={box}
      className="relative"
      onKeyDown={(e) => {
        if (e.key === 'Escape' && open) {
          e.stopPropagation()
          setOpen(false)
          button.current?.focus()
        }
      }}
    >
      {open && (
        // Picking an item closes the menu in the same click that starts the navigation. The effect on
        // the path below is the backstop (a link followed some other way), but it runs a render after
        // the URL changes, and until then the menu would still read as open.
        <div role="menu" aria-label="More pages" onClick={() => setOpen(false)} className="absolute bottom-full left-0 right-0 mb-1 z-50 rounded-lg border border-line bg-bg p-1 space-y-0.5">
          <NavLink role="menuitem" to="/media" className={({ isActive }) => item(isActive)}><ImageIcon size={14} /> Media</NavLink>
          <NavLink role="menuitem" to="/settings" className={({ isActive }) => item(isActive)}><SettingsIcon size={14} /> Settings</NavLink>
          <NavLink role="menuitem" to="/agents" className={({ isActive }) => item(isActive)}><Bot size={14} /> Agents</NavLink>
          <NavLink role="menuitem" to="/favorites" className={({ isActive }) => item(isActive)}><Star size={14} /> Favorites</NavLink>
          <NavLink role="menuitem" to="/notebooks" className={({ isActive }) => item(isActive)}><BookOpen size={14} /> Notebooks</NavLink>
          <NavLink role="menuitem" to="/scheduled" className={({ isActive }) => item(isActive)}><CalendarClock size={14} /> Scheduled</NavLink>
          {owner && (
            <>
              <NavLink role="menuitem" to="/jobs" className={({ isActive }) => item(isActive)}><SquareTerminal size={14} /> Jobs</NavLink>
              <NavLink role="menuitem" to="/training" className={({ isActive }) => item(isActive)}><FlaskConical size={14} /> Training</NavLink>
              <NavLink role="menuitem" to="/admin" className={({ isActive }) => item(isActive)}><Shield size={14} /> Admin</NavLink>
            </>
          )}
          <button role="menuitem" onClick={onSignOut} className={item(false)}><LogOut size={14} /> Sign out</button>
        </div>
      )}
      {/* The whole row is the button: the icon, the name and the empty space to the right of it. */}
      <button
        ref={button}
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={who ? `More, ${who}` : 'More'}
        title={email}
        className={clsx('w-full min-h-9 flex items-center gap-2 rounded-md px-2 text-left text-xs hover:bg-bg-3 hover:text-fg', open ? 'bg-bg-3 text-fg' : 'text-fg-3')}
      >
        <Menu size={16} className="shrink-0 text-fg-2" aria-hidden="true" />
        <span className="min-w-0 flex-1 truncate">{who}</span>
      </button>
    </div>
  )
}

export default function Shell() {
  const me = useMe()
  const nav = useNavigate()
  const qc = useQueryClient()
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })
  const convs = useQuery({ queryKey: ['convs'], queryFn: () => api.get<Conversation[]>('/api/conversations/recent') })

  const newChat = useMutation({
    mutationFn: async () => {
      // A conversation takes its mode from its project (a code project's conversations are code
      // conversations, with the code panel), so a plain chat goes in a chat project, never in
      // whichever project was touched last. Ask the server if the list has not arrived yet.
      const list: Project[] = projects.data ?? (await qc.fetchQuery<Project[]>({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })) ?? []
      let p = list.find((x) => x.kind === 'chat')
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

  // Below md the sidebar is a drawer over the page; it closes on navigation.
  const [drawer, setDrawer] = useState(false)
  const loc = useLocation()
  useEffect(() => setDrawer(false), [loc.pathname])

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
  const confirmDialog = useConfirm()
  const promptDialog = usePrompt()
  const newDesign = useMutation({
    mutationFn: async (name: string) => {
      const p = await api.post<Project>('/api/projects', { name, kind: 'design' })
      await qc.invalidateQueries({ queryKey: ['projects'] })
      return api.post<Conversation>(`/api/projects/${p.id}/conversations`, {})
    },
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ['convs'] })
      nav(`/c/${c.id}`)
    },
  })

  // Renaming a conversation edits its title in place in the list.
  const [editing, setEditing] = useState<{ id: string; draft: string } | null>(null)

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
      qc.invalidateQueries({ queryKey: ['archive'] })
      if (location.pathname === `/c/${id}`) nav('/')
    },
  })

  async function logout() {
    await api.post('/api/auth/logout')
    await qc.invalidateQueries({ queryKey: ['me'] })
    nav('/login')
  }

  return (
    <div className="h-full flex flex-col md:flex-row">
      <header className="md:hidden h-12 shrink-0 border-b border-line bg-bg-2 flex items-center gap-2 px-2">
        <button
          onClick={() => setDrawer(true)}
          className="p-2.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg"
          aria-label="Open menu"
          aria-expanded={drawer}
        >
          <Menu size={18} />
        </button>
        <Link to="/" className="wordmark inline-flex items-center gap-2">
          <WsMark size={22} className="text-fg" />
          ws
        </Link>
        <LocalityBadge info={me.data?.locality} stepUp={{ until: me.data?.step_up_until, hasPasskey: me.data?.has_passkey }} className="ml-auto" />
      </header>
      {drawer && <div className="md:hidden fixed inset-0 z-30 bg-bg/80" onClick={() => setDrawer(false)} aria-hidden="true" />}
      <aside
        className={clsx(
          'w-64 max-w-[85vw] shrink-0 border-r border-line bg-bg-2 flex flex-col',
          'fixed inset-y-0 left-0 z-40 md:static md:z-auto',
          drawer ? 'translate-x-0' : '-translate-x-full md:translate-x-0',
        )}
        aria-label="Sidebar"
      >
        <div className="px-3 pt-3 pb-2 flex items-center justify-between">
          <Link to="/" className="wordmark inline-flex items-center gap-2 min-h-10 md:min-h-0">
            <WsMark size={22} className="text-fg" />
            ws
          </Link>
          <LocalityBadge info={me.data?.locality} stepUp={{ until: me.data?.step_up_until, hasPasskey: me.data?.has_passkey }} className="ml-auto" />
          <button
            onClick={() => setDrawer(false)}
            className="md:hidden mr-1 p-2.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg"
            aria-label="Close menu"
          >
            <X size={18} />
          </button>
          <button
            onClick={() => newChat.mutate()}
            disabled={newChat.isPending}
            className="p-2.5 md:p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg disabled:opacity-60"
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
          <button
            onClick={async () => {
              const name = await promptDialog({ title: 'New design project', body: 'Conversations in it make designs under its design system.', label: 'Project name', initial: 'Design', confirm: 'Create project' })
              if (name) newDesign.mutate(name)
            }}
            disabled={newDesign.isPending}
            className={clsx(navItem({ isActive: false }), 'text-xs')}
          >
            <Palette size={14} /> New design project
          </button>
        </div>
        <div className="px-3">
          <h2 className="section-head">conversations</h2>
        </div>
        <nav className="flex-1 overflow-y-auto px-2 pt-2 space-y-0.5">
          {convs.data?.map((c) => (
            <div key={c.id} className="group relative">
              {editing?.id === c.id ? (
                <RenameField
                  value={editing.draft}
                  onChange={(draft) => setEditing({ id: c.id, draft })}
                  onSave={(title) => {
                    setEditing(null)
                    if (title && title !== (c.title || '')) rename.mutate({ id: c.id, title })
                  }}
                  onCancel={() => setEditing(null)}
                />
              ) : (
                <>
                  <NavLink to={`/c/${c.id}`} className={navItem} onDoubleClick={() => setEditing({ id: c.id, draft: c.title || '' })}>
                    <MessageSquare size={14} className="shrink-0" />
                    <span className="truncate pr-10">{c.title || 'New conversation'}</span>
                  </NavLink>
                  <span className="absolute right-1 top-1/2 -translate-y-1/2 flex items-center gap-0.5 md:opacity-0 md:group-hover:opacity-100 md:focus-within:opacity-100">
                    <button
                      onClick={() => setEditing({ id: c.id, draft: c.title || '' })}
                      className="p-2 md:p-1 min-h-9 min-w-9 md:min-h-0 md:min-w-0 inline-flex items-center justify-center rounded text-fg-3 hover:text-fg hover:bg-bg"
                      title="Rename"
                      aria-label="Rename conversation"
                    >
                      <Pencil size={12} />
                    </button>
                    <button
                      onClick={async () => {
                        if (await confirmDialog({ title: 'Archive this conversation?', body: `"${c.title || 'New conversation'}" leaves your list.`, confirm: 'Archive conversation' })) archive.mutate(c.id)
                      }}
                      className="p-2 md:p-1 min-h-9 min-w-9 md:min-h-0 md:min-w-0 inline-flex items-center justify-center rounded text-fg-3 hover:text-fg hover:bg-bg"
                      title="Archive"
                      aria-label="Archive conversation"
                    >
                      <Archive size={12} />
                    </button>
                  </span>
                </>
              )}
            </div>
          ))}
          {convs.data?.length === 0 && <p className="meta px-2 py-4">No conversations yet.</p>}
        </nav>
        <div className="p-2 border-t border-line text-sm space-y-0.5">
          <SidebarMenu owner={me.data?.role === 'owner'} who={me.data?.display_name || me.data?.email} email={me.data?.email} onSignOut={logout} />
        </div>
      </aside>
      <main className="flex-1 min-w-0 min-h-0 flex flex-col">
        <Outlet />
      </main>
    </div>
  )
}
