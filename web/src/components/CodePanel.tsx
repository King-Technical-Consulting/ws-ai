import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { EditorView, basicSetup } from 'codemirror'
import { EditorState, Compartment } from '@codemirror/state'
import { keymap } from '@codemirror/view'
import { javascript } from '@codemirror/lang-javascript'
import { python } from '@codemirror/lang-python'
import { go } from '@codemirror/lang-go'
import { markdown } from '@codemirror/lang-markdown'
import { json } from '@codemirror/lang-json'
import { html } from '@codemirror/lang-html'
import { css } from '@codemirror/lang-css'
import { yaml } from '@codemirror/lang-yaml'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { ChevronDown, ChevronRight, File as FileIcon, Folder, X, RefreshCw, ExternalLink } from 'lucide-react'
import clsx from 'clsx'
import { api } from '../api'
import { SectionHead, btn, inputSm } from './ui'

type Entry = { name: string; type: 'file' | 'dir' | 'link' | 'other'; size: number }
type Tree = { path: string; entries: Entry[] }
type FileResp = { path: string; binary: boolean; size: number; content: string }
type SandboxInfo = { id: string; status: string; runtime: string; container_id: string; repo_url: string | null }

type Tab = 'files' | 'terminal' | 'preview'

/**
 * Right-hand panel for code projects: file tree + editor, a terminal into
 * the sandbox, and dev-server previews. Everything goes through
 * /api/projects/{id}/… which serve relays to the worker.
 */
export function CodePanel({ projectId, onClose }: { projectId: string; onClose: () => void }) {
  const [tab, setTab] = useState<Tab>('files')
  const sandbox = useQuery({
    queryKey: ['sandbox', projectId],
    queryFn: () => api.get<SandboxInfo>(`/api/projects/${projectId}/sandbox`),
    staleTime: 30_000,
    retry: 1,
  })
  return (
    <aside className="fixed inset-0 z-20 md:static md:z-auto md:w-[46rem] md:max-w-[55vw] md:shrink-0 border-l border-line bg-bg flex flex-col min-h-0">
      <header className="h-12 shrink-0 border-b border-line flex items-center justify-between px-3 gap-2">
        <nav className="flex items-center gap-1">
          {(['files', 'terminal', 'preview'] as Tab[]).map((t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={clsx('section-head px-2 py-1 rounded-md border-0', tab === t ? 'bg-bg-3 text-fg' : 'text-fg-3 hover:text-fg')}
            >
              {t}
            </button>
          ))}
        </nav>
        <div className="flex items-center gap-2 min-w-0">
          <span className="meta truncate">
            {sandbox.isLoading ? 'starting sandbox…' : sandbox.data ? `${sandbox.data.status} · ${sandbox.data.runtime}` : sandbox.error ? (sandbox.error as Error).message : ''}
          </span>
          <button onClick={onClose} className="p-1 rounded-md text-fg-3 hover:text-fg hover:bg-bg-3" aria-label="Close panel"><X size={16} /></button>
        </div>
      </header>
      <div className="flex-1 min-h-0">
        {tab === 'files' && <Files projectId={projectId} ready={!!sandbox.data} />}
        {tab === 'terminal' && <Term projectId={projectId} ready={!!sandbox.data} />}
        {tab === 'preview' && <Preview projectId={projectId} />}
      </div>
    </aside>
  )
}

// ---- files ----

function Files({ projectId, ready }: { projectId: string; ready: boolean }) {
  const [open, setOpen] = useState<string | null>(null)
  return (
    <div className="h-full flex min-h-0">
      <div className="w-56 shrink-0 border-r border-line overflow-y-auto py-2 text-sm">
        {ready ? <Dir projectId={projectId} path="" depth={0} selected={open} onOpen={setOpen} /> : <p className="meta px-3">Waiting for the sandbox…</p>}
      </div>
      <div className="flex-1 min-w-0 min-h-0 flex flex-col">
        {open ? <Editor key={open} projectId={projectId} path={open} /> : <p className="meta p-4">Pick a file to view or edit. Cmd/Ctrl+S saves.</p>}
      </div>
    </div>
  )
}

function Dir({ projectId, path, depth, selected, onOpen }: { projectId: string; path: string; depth: number; selected: string | null; onOpen: (p: string) => void }) {
  const tree = useQuery({
    queryKey: ['tree', projectId, path],
    queryFn: () => api.get<Tree>(`/api/projects/${projectId}/fs/tree?path=${encodeURIComponent(path)}`),
    staleTime: 15_000,
  })
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const qc = useQueryClient()
  if (tree.isLoading) return <p className="meta px-3" style={{ paddingLeft: 12 + depth * 12 }}>…</p>
  if (tree.error) return <p className="text-xs text-danger px-3">{(tree.error as Error).message}</p>
  return (
    <ul>
      {depth === 0 && (
        <li className="px-2 pb-1 flex justify-end">
          <button onClick={() => qc.invalidateQueries({ queryKey: ['tree', projectId] })} className="p-1 text-fg-3 hover:text-fg" title="Refresh"><RefreshCw size={12} /></button>
        </li>
      )}
      {tree.data?.entries.map((e) => {
        const full = path ? `${path}/${e.name}` : e.name
        if (e.type === 'dir') {
          const isOpen = !!expanded[e.name]
          return (
            <li key={e.name}>
              <button
                onClick={() => setExpanded({ ...expanded, [e.name]: !isOpen })}
                className="w-full flex items-center gap-1.5 px-2 py-0.5 text-fg-2 hover:bg-bg-2 truncate"
                style={{ paddingLeft: 8 + depth * 12 }}
              >
                {isOpen ? <ChevronDown size={12} className="shrink-0" /> : <ChevronRight size={12} className="shrink-0" />}
                <Folder size={13} className="shrink-0 text-fg-3" />
                <span className="truncate">{e.name}</span>
              </button>
              {isOpen && <Dir projectId={projectId} path={full} depth={depth + 1} selected={selected} onOpen={onOpen} />}
            </li>
          )
        }
        return (
          <li key={e.name}>
            <button
              onClick={() => onOpen(full)}
              className={clsx('w-full flex items-center gap-1.5 px-2 py-0.5 truncate', selected === full ? 'bg-bg-3 text-fg' : 'text-fg-2 hover:bg-bg-2')}
              style={{ paddingLeft: 22 + depth * 12 }}
            >
              <FileIcon size={13} className="shrink-0 text-fg-3" />
              <span className="truncate">{e.name}</span>
            </button>
          </li>
        )
      })}
      {tree.data?.entries.length === 0 && <li className="meta px-3" style={{ paddingLeft: 12 + depth * 12 }}>empty</li>}
    </ul>
  )
}

function languageFor(path: string) {
  const ext = path.split('.').pop()?.toLowerCase() ?? ''
  switch (ext) {
    case 'js': case 'jsx': case 'mjs': case 'cjs': return javascript({ jsx: true })
    case 'ts': case 'tsx': case 'mts': return javascript({ jsx: true, typescript: true })
    case 'py': return python()
    case 'go': return go()
    case 'md': case 'mdx': return markdown()
    case 'json': return json()
    case 'html': case 'htm': case 'svg': case 'vue': return html()
    case 'css': case 'scss': return css()
    case 'yml': case 'yaml': return yaml()
    default: return []
  }
}

function Editor({ projectId, path }: { projectId: string; path: string }) {
  const host = useRef<HTMLDivElement>(null)
  const view = useRef<EditorView | null>(null)
  const [dirty, setDirty] = useState(false)
  const [status, setStatus] = useState<string>('')
  const file = useQuery({
    queryKey: ['file', projectId, path],
    queryFn: () => api.get<FileResp>(`/api/projects/${projectId}/fs/file?path=${encodeURIComponent(path)}`),
    staleTime: Infinity,
  })
  const qc = useQueryClient()
  const save = useCallback(async () => {
    if (!view.current) return
    setStatus('saving…')
    try {
      await api.put(`/api/projects/${projectId}/fs/file`, { path, content: view.current.state.doc.toString() })
      setDirty(false)
      setStatus('saved')
      qc.invalidateQueries({ queryKey: ['tree', projectId] })
      setTimeout(() => setStatus(''), 1500)
    } catch (e) {
      setStatus((e as Error).message)
    }
  }, [projectId, path, qc])

  const themeCompartment = useMemo(() => new Compartment(), [])
  useEffect(() => {
    if (!host.current || !file.data || file.data.binary) return
    const theme = EditorView.theme({
      '&': { height: '100%', fontSize: '13px', backgroundColor: 'var(--color-bg)', color: 'var(--color-fg)' },
      '.cm-scroller': { fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', overflow: 'auto' },
      '.cm-gutters': { backgroundColor: 'var(--color-bg-2)', color: 'var(--color-fg-4)', border: 'none' },
      '.cm-activeLine, .cm-activeLineGutter': { backgroundColor: 'var(--color-bg-2)' },
      '.cm-content': { caretColor: 'var(--color-fg)' },
      '&.cm-focused .cm-cursor': { borderLeftColor: 'var(--color-fg)' },
      '&.cm-focused .cm-selectionBackground, ::selection': { backgroundColor: 'var(--color-bg-3)' },
    })
    const state = EditorState.create({
      doc: file.data.content,
      extensions: [
        basicSetup,
        keymap.of([{ key: 'Mod-s', run: () => { void save(); return true } }]),
        languageFor(path),
        themeCompartment.of(theme),
        EditorView.updateListener.of((u) => { if (u.docChanged) setDirty(true) }),
      ],
    })
    const v = new EditorView({ state, parent: host.current })
    view.current = v
    return () => { v.destroy(); view.current = null }
  }, [file.data, path, save, themeCompartment])

  if (file.isLoading) return <p className="meta p-4">Loading {path}…</p>
  if (file.error) return <p className="text-sm text-danger p-4">{(file.error as Error).message}</p>
  if (file.data?.binary) return <p className="meta p-4">{path}: binary file, {file.data.size} bytes.</p>
  return (
    <>
      <div className="h-9 shrink-0 border-b border-line flex items-center justify-between px-3 gap-2">
        <span className="font-mono text-xs text-fg-2 truncate">{path}{dirty ? ' •' : ''}</span>
        <div className="flex items-center gap-2">
          <span className="meta">{status}</span>
          <button onClick={() => void save()} disabled={!dirty} className={clsx(btn.secondarySm, 'py-0.5 px-2 text-xs disabled:opacity-50')}>Save</button>
        </div>
      </div>
      <div ref={host} className="flex-1 min-h-0" />
    </>
  )
}

// ---- terminal ----

function Term({ projectId, ready }: { projectId: string; ready: boolean }) {
  const host = useRef<HTMLDivElement>(null)
  const [state, setState] = useState<'connecting' | 'open' | 'closed'>('connecting')
  const [gen, setGen] = useState(0)
  useEffect(() => {
    if (!host.current || !ready) return
    const cs = getComputedStyle(document.documentElement)
    const term = new Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      cursorBlink: true,
      theme: {
        background: cs.getPropertyValue('--color-bg').trim() || '#1b1611',
        foreground: cs.getPropertyValue('--color-fg').trim() || '#e8e2d8',
        cursor: cs.getPropertyValue('--color-fg').trim() || '#e8e2d8',
        selectionBackground: cs.getPropertyValue('--color-bg-3').trim() || '#2e261f',
      },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(host.current)
    fit.fit()
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const ws = new WebSocket(`${proto}://${location.host}/api/projects/${projectId}/pty?cols=${term.cols}&rows=${term.rows}`)
    ws.binaryType = 'arraybuffer'
    const enc = new TextEncoder()
    ws.onopen = () => setState('open')
    ws.onmessage = (ev) => { if (ev.data instanceof ArrayBuffer) term.write(new Uint8Array(ev.data)) }
    ws.onclose = () => { setState('closed'); term.write('\r\n\x1b[2m[session closed]\x1b[0m\r\n') }
    ws.onerror = () => setState('closed')
    const onData = term.onData((d) => { if (ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d)) })
    const onResize = term.onResize(({ cols, rows }) => { if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'resize', cols, rows })) })
    const ro = new ResizeObserver(() => { try { fit.fit() } catch {} })
    ro.observe(host.current)
    return () => { ro.disconnect(); onData.dispose(); onResize.dispose(); ws.close(); term.dispose() }
  }, [projectId, ready, gen])
  return (
    <div className="h-full flex flex-col min-h-0">
      <div className="h-9 shrink-0 border-b border-line flex items-center justify-between px-3">
        <span className="meta">bash in /workspace · {ready ? state : 'waiting for the sandbox'}</span>
        {state === 'closed' && <button onClick={() => { setState('connecting'); setGen((g) => g + 1) }} className={clsx(btn.secondarySm, 'py-0.5 px-2 text-xs')}>Reconnect</button>}
      </div>
      <div ref={host} className="flex-1 min-h-0 p-1" />
    </div>
  )
}

// ---- preview ----

function Preview({ projectId }: { projectId: string }) {
  const [port, setPort] = useState('3000')
  const href = `/api/projects/${projectId}/preview/${port}`
  return (
    <div className="p-4 space-y-3 text-sm">
      <SectionHead>dev server preview</SectionHead>
      <p className="reading text-fg-2">Start a dev server in the terminal (or ask the assistant to), then open its port here. The preview runs on its own hostname with a signed cookie, so the page can't read your ws session.</p>
      <form className="flex items-center gap-2" onSubmit={(e) => { e.preventDefault(); window.open(href, '_blank', 'noopener') }}>
        <label className="text-fg-2">Port</label>
        <input value={port} onChange={(e) => setPort(e.target.value.replace(/\D/g, ''))} className={`${inputSm} w-24`} inputMode="numeric" />
        <button type="submit" className={btn.primarySm}><ExternalLink size={14} className="inline mr-1" />Open preview</button>
      </form>
      <p className="meta">Bind the server to 0.0.0.0 (Vite: <code className="font-mono">--host</code>) so the proxy can reach it.</p>
    </div>
  )
}
