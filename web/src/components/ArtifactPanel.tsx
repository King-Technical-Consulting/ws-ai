import { createContext, useContext, useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ChevronLeft, ChevronRight, ExternalLink, X, FileCode2 } from 'lucide-react'
import { api } from '../api'
import clsx from 'clsx'

export type ArtifactRef = { artifact_id: string; version: number; version_id: string; kind: string; title: string; url: string }

type ArtifactDetail = {
  artifact: { id: string; kind: string; title: string; current_version: number }
  versions: { version: number; created_at: string }[]
  version: number
  version_id: string
  url: string
  content: string
  kind: string
  title: string
}

export const ArtifactContext = createContext<{ open: (id: string, version?: number) => void }>({ open: () => {} })

export function useOpenArtifact() {
  return useContext(ArtifactContext).open
}

/** Clickable card rendered inside a reply for create_artifact / update_artifact outputs. */
export function ArtifactCard({ output, state }: { output?: unknown; state: string }) {
  const open = useOpenArtifact()
  const ref = output as ArtifactRef | undefined
  if (!ref || !ref.artifact_id) {
    return (
      <div className="my-2 rounded-lg border border-line px-3 py-2 font-sans text-sm text-fg-2 flex items-center gap-2">
        <FileCode2 size={14} /> {state === 'output-error' ? 'Artifact failed.' : 'Writing artifact…'}
      </div>
    )
  }
  return (
    <button
      onClick={() => open(ref.artifact_id, ref.version)}
      className="my-2 w-full text-left rounded-lg border border-line bg-bg-2 hover:bg-bg-3 px-3 py-2 font-sans text-sm flex items-center gap-3"
    >
      <FileCode2 size={16} className="text-fg-3 shrink-0" />
      <span className="min-w-0 flex-1">
        <span className="block truncate text-fg">{ref.title}</span>
        <span className="meta">{ref.kind} · v{ref.version}</span>
      </span>
      <ChevronRight size={14} className="text-fg-3" />
    </button>
  )
}

export function ArtifactPanel({ id, version, onClose, onVersion }: { id: string; version?: number; onClose: () => void; onVersion: (v: number) => void }) {
  const q = useQuery({
    queryKey: ['artifact', id, version ?? 0],
    queryFn: () => api.get<ArtifactDetail>(`/api/artifacts/${id}${version ? `?version=${version}` : ''}`),
  })
  const frame = useRef<HTMLIFrameElement>(null)
  const [frameErr, setFrameErr] = useState<string | null>(null)

  // Messages from the sandboxed document. Its origin is "null", so we
  // check the source window instead.
  useEffect(() => {
    function onMsg(e: MessageEvent) {
      if (!frame.current || e.source !== frame.current.contentWindow) return
      const d = e.data as { ws?: string; type?: string; message?: string; href?: string }
      if (d?.ws !== 'artifact') return
      if (d.type === 'error') setFrameErr(d.message ?? 'error')
      if (d.type === 'open' && d.href) window.open(d.href, '_blank', 'noopener,noreferrer')
    }
    window.addEventListener('message', onMsg)
    return () => window.removeEventListener('message', onMsg)
  }, [])
  useEffect(() => setFrameErr(null), [q.data?.version_id])

  const d = q.data
  const total = d?.artifact.current_version ?? 0
  const cur = d?.version ?? 0

  return (
    <aside className="w-[46%] min-w-[380px] shrink-0 border-l border-line bg-bg-2 flex flex-col">
      <header className="h-12 shrink-0 border-b border-line flex items-center gap-2 px-3">
        <span className="min-w-0 flex-1 truncate reading-tight">{d?.title ?? 'Artifact'}</span>
        {d && <span className="meta">{d.kind}</span>}
        <div className="flex items-center gap-0.5 text-fg-2">
          <button disabled={cur <= 1} onClick={() => onVersion(cur - 1)} className="p-1.5 rounded-md hover:bg-bg-3 disabled:opacity-40" title="Previous version" aria-label="Previous version">
            <ChevronLeft size={14} />
          </button>
          <span className="text-xs tnum text-fg-3 min-w-[3.5rem] text-center">v{cur} / {total}</span>
          <button disabled={cur >= total} onClick={() => onVersion(cur + 1)} className="p-1.5 rounded-md hover:bg-bg-3 disabled:opacity-40" title="Next version" aria-label="Next version">
            <ChevronRight size={14} />
          </button>
        </div>
        {d && (
          <a href={d.url} target="_blank" rel="noopener noreferrer" className="p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg" title="Open in new tab" aria-label="Open in new tab">
            <ExternalLink size={16} />
          </a>
        )}
        <button onClick={onClose} className="p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg" title="Close" aria-label="Close">
          <X size={16} />
        </button>
      </header>
      <div className="flex-1 min-h-0 relative bg-bg">
        {q.isLoading && <p className="meta p-4">Loading…</p>}
        {q.error && <p className="text-sm text-danger p-4">Could not load this artifact.</p>}
        {d && d.kind === 'code' ? (
          <pre className="h-full overflow-auto m-0 p-4 font-mono text-xs leading-relaxed">{d.content}</pre>
        ) : d ? (
          <iframe
            ref={frame}
            key={d.version_id}
            title={d.title}
            src={d.url}
            sandbox="allow-scripts"
            referrerPolicy="no-referrer"
            className={clsx('w-full h-full border-0 bg-bg')}
          />
        ) : null}
        {frameErr && (
          <div className="absolute bottom-0 inset-x-0 border-t border-danger/40 bg-bg-2 px-3 py-1.5 font-mono text-xs text-danger truncate" title={frameErr}>
            {frameErr}
          </div>
        )}
      </div>
    </aside>
  )
}
