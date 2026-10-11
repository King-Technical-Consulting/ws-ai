import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router'
import { api } from '../api'
import { SectionHead, PageTitle, Callout, ListGroup, btn, inputSm } from '../components/ui'

type Profile = { name?: string; address_as?: string; role?: string; projects?: string[]; tools?: string[]; tone?: string; this_week?: string }
type MemorySettings = {
  enabled: boolean
  reflect_enabled: boolean
  reflect_at: string
  timezone: string
  profile: Profile
  last_reflected_at: string | null
}
type Mem = { id: string; kind: 'fact' | 'preference' | 'decision'; content: string; source: string; importance: number; last_used_at: string | null; created_at: string }
type MemoryResponse = { settings: MemorySettings; memories: Mem[]; total: number }

const list = (s: string) => s.split(',').map((x) => x.trim()).filter(Boolean)

export default function Memory() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['memory'], queryFn: () => api.get<MemoryResponse>('/api/memory') })
  const [set, setSet] = useState<MemorySettings | null>(null)
  const [projects, setProjects] = useState('')
  const [tools, setTools] = useState('')
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!q.data) return
    setSet(q.data.settings)
    setProjects((q.data.settings.profile.projects ?? []).join(', '))
    setTools((q.data.settings.profile.tools ?? []).join(', '))
  }, [q.data])

  const refresh = () => qc.invalidateQueries({ queryKey: ['memory'] })
  const fail = (e: unknown) => setError(e instanceof Error ? e.message : 'Something went wrong.')

  const save = useMutation({
    mutationFn: (s: MemorySettings) => api.put<MemorySettings>('/api/memory/settings', s),
    onSuccess: () => {
      setSaved(true)
      setError('')
      refresh()
    },
    onError: fail,
  })
  const tidy = useMutation({ mutationFn: () => api.post('/api/memory/reflect'), onError: fail })
  const add = useMutation({
    mutationFn: (b: { kind: string; content: string }) => api.post('/api/memory', b),
    onSuccess: refresh,
    onError: fail,
  })
  const del = useMutation({ mutationFn: (id: string) => api.del(`/api/memory/${id}`), onSuccess: refresh, onError: fail })
  const edit = useMutation({
    mutationFn: (m: Mem) => api.patch(`/api/memory/${m.id}`, { kind: m.kind, content: m.content, importance: m.importance }),
    onSuccess: refresh,
    onError: fail,
  })
  const forget = useMutation({ mutationFn: () => api.del('/api/memory'), onSuccess: refresh, onError: fail })

  const [newKind, setNewKind] = useState('fact')
  const [newText, setNewText] = useState('')
  const [confirmForget, setConfirmForget] = useState(false)

  if (q.isLoading || !set) return <div className="p-6 meta">Loading…</div>
  if (q.error) return <div className="p-6"><Callout kind="error">Memory is not available on this server.</Callout></div>

  const profile = set.profile
  const setProfile = (p: Partial<Profile>) => {
    setSaved(false)
    setSet({ ...set, profile: { ...profile, ...p } })
  }
  const onSave = () => save.mutate({ ...set, profile: { ...profile, projects: list(projects), tools: list(tools) } })

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-2xl p-6 space-y-10">
        <div>
          <p className="meta"><Link to="/settings" className="text-accent border-b border-dotted border-link-underline hover:text-accent-hover">Settings</Link></p>
          <PageTitle>Memory</PageTitle>
          <p className="tagline mt-1">What ws keeps about you between chats. It is yours alone: nobody else sees it, and it is not used for training.</p>
        </div>

        {error && <Callout kind="error">{error}</Callout>}

        <section className="space-y-3">
          <SectionHead>switches</SectionHead>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={set.enabled} onChange={(e) => { setSaved(false); setSet({ ...set, enabled: e.target.checked }) }} />
            Remember things for me and use them in my chats
          </label>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={set.reflect_enabled} disabled={!set.enabled} onChange={(e) => { setSaved(false); setSet({ ...set, reflect_enabled: e.target.checked }) }} />
            Tidy up every night: read the day's chats, keep what is new, merge repeats, let unused things fade
          </label>
          <div className="flex flex-wrap items-end gap-3 text-sm">
            <div>
              <label htmlFor="reflect-at" className="meta block">Time of day</label>
              <input id="reflect-at" type="time" value={set.reflect_at} onChange={(e) => { setSaved(false); setSet({ ...set, reflect_at: e.target.value }) }} className={inputSm} />
            </div>
            <div className="grow">
              <label htmlFor="reflect-tz" className="meta block">Time zone</label>
              <input id="reflect-tz" value={set.timezone} onChange={(e) => { setSaved(false); setSet({ ...set, timezone: e.target.value }) }} className={inputSm} placeholder="Europe/Berlin" />
            </div>
            <button className={btn.secondarySm} onClick={() => tidy.mutate()} disabled={!set.enabled || tidy.isPending}>Tidy up now</button>
          </div>
          {tidy.isSuccess && <p className="meta">Queued. New memories appear here when it finishes.</p>}
          {set.last_reflected_at && <p className="meta">Last tidied {new Date(set.last_reflected_at).toLocaleString()}.</p>}
        </section>

        <section className="space-y-3">
          <SectionHead>about you</SectionHead>
          <p className="meta">A short card the model reads at the start of every chat, before any memories. Edit it freely.</p>
          <div className="grid gap-3 sm:grid-cols-2 text-sm">
            <Field id="card-name" label="Name" value={profile.name ?? ''} onChange={(v) => setProfile({ name: v })} />
            <Field id="card-address" label="Address you as" value={profile.address_as ?? ''} onChange={(v) => setProfile({ address_as: v })} />
            <Field id="card-role" label="Role" value={profile.role ?? ''} onChange={(v) => setProfile({ role: v })} />
            <Field id="card-tone" label="How you like answers" value={profile.tone ?? ''} onChange={(v) => setProfile({ tone: v })} />
            <Field id="card-projects" label="Projects (comma separated)" value={projects} onChange={(v) => { setSaved(false); setProjects(v) }} />
            <Field id="card-tools" label="Tools and stack (comma separated)" value={tools} onChange={(v) => { setSaved(false); setTools(v) }} />
            <div className="sm:col-span-2">
              <Field id="card-week" label="What you are working on this week" value={profile.this_week ?? ''} onChange={(v) => setProfile({ this_week: v })} />
            </div>
          </div>
          <div className="flex items-center gap-3">
            <button className={btn.primarySm} onClick={onSave} disabled={save.isPending}>Save changes</button>
            {saved && <span role="status" className="meta">Saved.</span>}
          </div>
        </section>

        <section className="space-y-3">
          <SectionHead aside={`${q.data?.total ?? 0} kept`}>memories</SectionHead>
          <ListGroup empty="Nothing yet. Tell ws something worth keeping in a chat, or add it here.">
            {(q.data?.memories ?? []).map((m) => (
              <MemoryRow key={m.id} m={m} onSave={(x) => edit.mutate(x)} onDelete={() => del.mutate(m.id)} />
            ))}
          </ListGroup>
          <form
            className="flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              if (!newText.trim()) return
              add.mutate({ kind: newKind, content: newText.trim() }, { onSuccess: () => setNewText('') })
            }}
          >
            <div className="grow min-w-48">
              <label htmlFor="new-memory" className="meta block">Add a memory</label>
              <input id="new-memory" value={newText} onChange={(e) => setNewText(e.target.value)} className={inputSm} maxLength={500} placeholder="I prefer metric units" />
            </div>
            <select aria-label="Kind" value={newKind} onChange={(e) => setNewKind(e.target.value)} className={inputSm + ' !w-auto'}>
              <option value="fact">fact</option>
              <option value="preference">preference</option>
              <option value="decision">decision</option>
            </select>
            <button type="submit" className={btn.secondarySm} disabled={!newText.trim() || add.isPending}>Add</button>
          </form>
          {(q.data?.total ?? 0) > 0 && (
            <div className="pt-2">
              {confirmForget ? (
                <span className="text-sm">
                  Delete all {q.data?.total} memories?{' '}
                  <button className={btn.danger} onClick={() => { forget.mutate(); setConfirmForget(false) }}>Yes, forget everything</button>{' '}
                  <button className="text-xs text-fg-2" onClick={() => setConfirmForget(false)}>Cancel</button>
                </span>
              ) : (
                <button className={btn.danger} onClick={() => setConfirmForget(true)}>Forget everything</button>
              )}
            </div>
          )}
        </section>
      </div>
    </div>
  )
}

function Field({ id, label, value, onChange }: { id: string; label: string; value: string; onChange: (v: string) => void }) {
  return (
    <div>
      <label htmlFor={id} className="meta block">{label}</label>
      <input id={id} value={value} onChange={(e) => onChange(e.target.value)} className={inputSm} />
    </div>
  )
}

function MemoryRow({ m, onSave, onDelete }: { m: Mem; onSave: (m: Mem) => void; onDelete: () => void }) {
  const [editing, setEditing] = useState(false)
  const [text, setText] = useState(m.content)
  const [kind, setKind] = useState(m.kind)
  if (editing) {
    return (
      <li className="px-3 py-2 space-y-2">
        <input aria-label="Memory text" value={text} onChange={(e) => setText(e.target.value)} className={inputSm} maxLength={500} />
        <div className="flex items-center gap-2">
          <select aria-label="Kind" value={kind} onChange={(e) => setKind(e.target.value as Mem['kind'])} className={inputSm + ' !w-auto'}>
            <option value="fact">fact</option>
            <option value="preference">preference</option>
            <option value="decision">decision</option>
          </select>
          <button className={btn.primarySm} onClick={() => { onSave({ ...m, content: text, kind }); setEditing(false) }}>Save</button>
          <button className={btn.secondarySm} onClick={() => { setText(m.content); setKind(m.kind); setEditing(false) }}>Cancel</button>
        </div>
      </li>
    )
  }
  return (
    <li className="flex items-start justify-between gap-3 px-3 py-2">
      <div className="min-w-0">
        <p className="reading-tight">{m.content}</p>
        <p className="meta">
          {m.kind} · {m.source === 'reflection' ? 'tidied from a chat' : m.source === 'manual' ? 'added by you' : 'from a chat'}
          {m.last_used_at ? ` · used ${new Date(m.last_used_at).toLocaleDateString()}` : ' · not used yet'}
        </p>
      </div>
      <div className="flex shrink-0 gap-3 text-xs">
        <button className="text-accent hover:text-accent-hover" onClick={() => setEditing(true)} aria-label={`Edit: ${m.content}`}>Edit</button>
        <button className={btn.danger} onClick={onDelete} aria-label={`Delete: ${m.content}`}>Delete</button>
      </div>
    </li>
  )
}
