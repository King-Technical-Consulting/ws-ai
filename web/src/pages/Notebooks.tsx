import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Conversation } from '../api'
import { SectionHead, PageTitle, Callout, ListGroup, btn, inputSm, useConfirm } from '../components/ui'

type NotebookSummary = { id: string; name: string; conversation_id: string | null; sources: number; updated_at: string }
type Source = { id: string; kind: 'document' | 'page' | 'text' | 'conversation'; title: string; url?: string; pages: number; chars: number }
type Note = { id: string; kind: 'summary' | 'open_questions' | 'note'; title: string; body: string; author: 'user' | 'model'; updated_at: string }
type Detail = { notebook: NotebookSummary; sources: Source[]; notes: Note[] }
type SourceText = { source: Source; pages: { page: number; text: string }[] }

/** The notebooks list at /notebooks and one notebook at /notebooks/:id. */
export default function Notebooks() {
  const { id } = useParams()
  return id ? <NotebookView id={id} /> : <NotebookList />
}

function NotebookList() {
  const qc = useQueryClient()
  const nav = useNavigate()
  const list = useQuery({ queryKey: ['notebooks'], queryFn: () => api.get<{ notebooks: NotebookSummary[] }>('/api/notebooks') })
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const create = useMutation({
    mutationFn: () => api.post<NotebookSummary>('/api/notebooks', { name }),
    onSuccess: (nb) => {
      qc.invalidateQueries({ queryKey: ['notebooks'] })
      nav(`/notebooks/${nb.id}`)
    },
    onError: (e) => setError(e instanceof Error ? e.message : 'Something went wrong.'),
  })
  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-2xl p-6 space-y-10">
        <div>
          <PageTitle>Notebooks</PageTitle>
          <p className="tagline mt-1">Collect sources, ask questions of them, and keep notes. Replies cite where they got things.</p>
        </div>
        {error && <Callout kind="error">{error}</Callout>}
        <section className="space-y-3">
          <SectionHead aside={`${list.data?.notebooks.length ?? 0} of 20`}>your notebooks</SectionHead>
          <ListGroup empty="No notebooks yet.">
            {(list.data?.notebooks ?? []).map((n) => (
              <li key={n.id} className="flex items-center justify-between gap-3 px-3 py-2">
                <Link to={`/notebooks/${n.id}`} className="min-w-0 truncate text-accent hover:text-accent-hover">{n.name}</Link>
                <span className="meta shrink-0">{n.sources} source{n.sources === 1 ? '' : 's'}</span>
              </li>
            ))}
          </ListGroup>
          <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              if (name.trim()) create.mutate()
            }}
          >
            <div className="grow">
              <label htmlFor="nb-name" className="meta block">New notebook</label>
              <input id="nb-name" value={name} onChange={(e) => setName(e.target.value)} className={inputSm} maxLength={120} placeholder="Heat pump research" />
            </div>
            <button type="submit" className={btn.primarySm} disabled={!name.trim() || create.isPending}>Create</button>
          </form>
        </section>
      </div>
    </div>
  )
}

function NotebookView({ id }: { id: string }) {
  const confirmDialog = useConfirm()
  const qc = useQueryClient()
  const nav = useNavigate()
  const q = useQuery({ queryKey: ['notebook', id], queryFn: () => api.get<Detail>(`/api/notebooks/${id}`) })
  const convs = useQuery({ queryKey: ['conversations-all'], queryFn: () => api.get<Conversation[]>('/api/conversations/recent') })
  const [error, setError] = useState('')
  const refresh = () => qc.invalidateQueries({ queryKey: ['notebook', id] })
  const fail = (e: unknown) => setError(e instanceof Error ? e.message : 'Something went wrong.')
  const fileRef = useRef<HTMLInputElement>(null)
  const [url, setUrl] = useState('')
  const [pasteTitle, setPasteTitle] = useState('')
  const [pasteText, setPasteText] = useState('')
  const [convId, setConvId] = useState('')
  const [noteTitle, setNoteTitle] = useState('')
  const [noteBody, setNoteBody] = useState('')
  const [openSource, setOpenSource] = useState<string | null>(null)

  // #source-<id> or #source-<id>-p<page> from a citation in the chat.
  useEffect(() => {
    const m = /^#source-([0-9a-f-]{36})/.exec(window.location.hash)
    if (m) setOpenSource(m[1])
  }, [id, q.data])

  const addJSON = useMutation({
    mutationFn: (b: Record<string, string>) => api.post(`/api/notebooks/${id}/sources`, b),
    onSuccess: () => {
      setUrl('')
      setPasteText('')
      setPasteTitle('')
      setError('')
      refresh()
    },
    onError: fail,
  })
  const addFile = useMutation({
    mutationFn: async (f: File) => {
      const fd = new FormData()
      fd.append('file', f)
      const res = await fetch(`/api/notebooks/${id}/sources`, { method: 'POST', body: fd, credentials: 'same-origin' })
      if (!res.ok) throw new Error(((await res.json().catch(() => ({}))) as { error?: string }).error ?? 'Could not read that file.')
    },
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: fail,
  })
  const delSource = useMutation({ mutationFn: (sid: string) => api.del(`/api/notebooks/${id}/sources/${sid}`), onSuccess: refresh, onError: fail })
  const addNote = useMutation({
    mutationFn: () => api.post(`/api/notebooks/${id}/notes`, { title: noteTitle, body: noteBody }),
    onSuccess: () => {
      setNoteTitle('')
      setNoteBody('')
      refresh()
    },
    onError: fail,
  })
  const delNote = useMutation({ mutationFn: (nid: string) => api.del(`/api/notebooks/${id}/notes/${nid}`), onSuccess: refresh, onError: fail })
  const openChat = useMutation({
    mutationFn: () => api.post<{ conversation_id: string }>(`/api/notebooks/${id}/chat`),
    onSuccess: (r) => nav(`/c/${r.conversation_id}`),
    onError: fail,
  })
  const delNotebook = useMutation({
    mutationFn: () => api.del(`/api/notebooks/${id}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notebooks'] })
      nav('/notebooks')
    },
    onError: fail,
  })

  if (q.isLoading) return <div className="p-6 meta">Loading…</div>
  if (q.error || !q.data) return <div className="p-6"><Callout kind="error">That notebook was not found.</Callout></div>
  const { notebook, sources, notes } = q.data
  const summary = notes.find((n) => n.kind === 'summary')
  const questions = notes.find((n) => n.kind === 'open_questions')
  const own = notes.filter((n) => n.kind === 'note')

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-2xl p-6 space-y-10">
        <div>
          <p className="meta"><Link to="/notebooks" className="text-accent border-b border-dotted border-link-underline hover:text-accent-hover">Notebooks</Link></p>
          <PageTitle>{notebook.name}</PageTitle>
          <div className="mt-3 flex gap-3">
            <button className={btn.primarySm} onClick={() => openChat.mutate()} disabled={openChat.isPending}>Open chat</button>
            <button className={btn.danger} onClick={async () => { if (await confirmDialog({ title: 'Delete this notebook?', body: 'Its sources and notes go with it.', confirm: 'Delete notebook', danger: true })) delNotebook.mutate() }}>Delete notebook</button>
          </div>
        </div>
        {error && <Callout kind="error">{error}</Callout>}

        <section className="space-y-3">
          <SectionHead aside={`${sources.length} of 30`}>sources</SectionHead>
          <ListGroup empty="No sources yet. Add a file, a page, some text or a conversation below.">
            {sources.map((s) => (
              <li key={s.id} id={`source-${s.id}`} className="px-3 py-2">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <p className="truncate">{s.title}</p>
                    <p className="meta">{s.kind} · {s.pages} page{s.pages === 1 ? '' : 's'} · {s.chars.toLocaleString()} characters</p>
                  </div>
                  <div className="flex shrink-0 gap-3 text-xs">
                    <button className="text-accent hover:text-accent-hover" onClick={() => setOpenSource(openSource === s.id ? null : s.id)} aria-expanded={openSource === s.id}>
                      {openSource === s.id ? 'Hide' : 'Read'}
                    </button>
                    <button className={btn.danger} onClick={() => delSource.mutate(s.id)} aria-label={`Remove source: ${s.title}`}>Remove</button>
                  </div>
                </div>
                {openSource === s.id && <SourceBody notebookId={id} sourceId={s.id} />}
              </li>
            ))}
          </ListGroup>
          <div className="space-y-3 text-sm">
            <div className="flex flex-wrap items-end gap-2">
              <div>
                <label htmlFor="nb-file" className="meta block">Add a file</label>
                <input
                  id="nb-file"
                  ref={fileRef}
                  type="file"
                  accept=".pdf,.docx,.xlsx,.pptx,.txt,.md,.csv,.json,image/*"
                  onChange={(e) => {
                    const f = e.target.files?.[0]
                    if (f) addFile.mutate(f)
                    if (fileRef.current) fileRef.current.value = ''
                  }}
                />
              </div>
            </div>
            <form className="flex items-end gap-2" onSubmit={(e) => { e.preventDefault(); if (url.trim()) addJSON.mutate({ url: url.trim() }) }}>
              <div className="grow">
                <label htmlFor="nb-url" className="meta block">Add a web page</label>
                <input id="nb-url" value={url} onChange={(e) => setUrl(e.target.value)} className={inputSm} placeholder="https://" />
              </div>
              <button type="submit" className={btn.secondarySm} disabled={!url.trim() || addJSON.isPending}>Fetch</button>
            </form>
            <form className="space-y-2" onSubmit={(e) => { e.preventDefault(); if (pasteText.trim()) addJSON.mutate({ title: pasteTitle, text: pasteText }) }}>
              <label htmlFor="nb-paste" className="meta block">Paste text</label>
              <input aria-label="Title for pasted text" value={pasteTitle} onChange={(e) => setPasteTitle(e.target.value)} className={inputSm} placeholder="Title (optional)" />
              <textarea id="nb-paste" value={pasteText} onChange={(e) => setPasteText(e.target.value)} rows={4} className={inputSm + ' reading'} />
              <button type="submit" className={btn.secondarySm} disabled={!pasteText.trim() || addJSON.isPending}>Add text</button>
            </form>
            <form className="flex items-end gap-2" onSubmit={(e) => { e.preventDefault(); if (convId) addJSON.mutate({ conversation_id: convId }) }}>
              <div className="grow">
                <label htmlFor="nb-conv" className="meta block">Add a conversation</label>
                <select id="nb-conv" value={convId} onChange={(e) => setConvId(e.target.value)} className={inputSm}>
                  <option value="">Choose one…</option>
                  {(convs.data ?? []).filter((c) => c.id !== notebook.conversation_id).map((c) => (
                    <option key={c.id} value={c.id}>{c.title || 'Untitled'}</option>
                  ))}
                </select>
              </div>
              <button type="submit" className={btn.secondarySm} disabled={!convId || addJSON.isPending}>Add</button>
            </form>
          </div>
        </section>

        <section className="space-y-3">
          <SectionHead>what the sources say</SectionHead>
          <NoteBlock label="Summary" note={summary} empty="Ask the chat to summarise the sources and it will keep this up to date." />
          <NoteBlock label="Open questions" note={questions} empty="Questions the sources leave open appear here." />
        </section>

        <section className="space-y-3">
          <SectionHead aside={`${own.length}`}>your notes</SectionHead>
          <ListGroup empty="No notes yet.">
            {own.map((n) => (
              <li key={n.id} className="flex items-start justify-between gap-3 px-3 py-2">
                <div className="min-w-0">
                  {n.title && <p>{n.title}</p>}
                  <p className="reading-tight whitespace-pre-wrap">{n.body}</p>
                  <p className="meta">{n.author === 'model' ? 'written by the model' : 'written by you'}</p>
                </div>
                <button className={btn.danger} onClick={() => delNote.mutate(n.id)} aria-label={`Delete note: ${n.title || n.body.slice(0, 20)}`}>Delete</button>
              </li>
            ))}
          </ListGroup>
          <form className="space-y-2 text-sm" onSubmit={(e) => { e.preventDefault(); if (noteBody.trim()) addNote.mutate() }}>
            <input aria-label="Note title" value={noteTitle} onChange={(e) => setNoteTitle(e.target.value)} className={inputSm} placeholder="Title (optional)" />
            <textarea aria-label="Note" value={noteBody} onChange={(e) => setNoteBody(e.target.value)} rows={3} className={inputSm + ' reading'} placeholder="A note" />
            <button type="submit" className={btn.secondarySm} disabled={!noteBody.trim() || addNote.isPending}>Add note</button>
          </form>
        </section>
      </div>
    </div>
  )
}

function NoteBlock({ label, note, empty }: { label: string; note?: Note; empty: string }) {
  return (
    <div className="rounded-lg border border-line bg-bg-2 px-3 py-2">
      <p className="meta">{label}{note ? ` · ${note.author === 'model' ? 'kept by the model' : 'edited by you'}` : ''}</p>
      {note ? <p className="reading-tight whitespace-pre-wrap mt-1">{note.body}</p> : <p className="meta mt-1">{empty}</p>}
    </div>
  )
}

function SourceBody({ notebookId, sourceId }: { notebookId: string; sourceId: string }) {
  const q = useQuery({ queryKey: ['notebook-source', notebookId, sourceId], queryFn: () => api.get<SourceText>(`/api/notebooks/${notebookId}/sources/${sourceId}`) })
  if (q.isLoading) return <p className="meta mt-2">Loading…</p>
  if (!q.data) return null
  return (
    <div className="mt-2 max-h-80 overflow-auto rounded-md border border-line bg-bg px-3 py-2">
      {q.data.pages.map((p) => (
        <div key={p.page} id={`source-${sourceId}-p${p.page}`} className="mb-3">
          <p className="meta">page {p.page}</p>
          <p className="reading-tight whitespace-pre-wrap">{p.text}</p>
        </div>
      ))}
    </div>
  )
}
