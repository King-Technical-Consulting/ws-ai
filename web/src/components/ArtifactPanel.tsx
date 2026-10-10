import { createContext, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { ChevronLeft, ChevronRight, Download, ExternalLink, GitCompare, Sparkles, X, FileCode2 } from 'lucide-react'
import { api, type DesignContext } from '../api'
import { btn, inputSm, Callout } from './ui'
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
  design_context: DesignContext | null
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

const TEXT_KINDS = new Set(['html', 'design', 'svg', 'markdown', 'mermaid', 'code'])

export function ArtifactPanel({ id, version, onClose, onVersion }: { id: string; version?: number; onClose: () => void; onVersion: (v: number) => void }) {
  const q = useQuery({
    queryKey: ['artifact', id, version ?? 0],
    queryFn: () => api.get<ArtifactDetail>(`/api/artifacts/${id}${version ? `?version=${version}` : ''}`),
  })
  const frame = useRef<HTMLIFrameElement>(null)
  const [frameErr, setFrameErr] = useState<string | null>(null)
  const [mode, setMode] = useState<'view' | 'diff' | 'variants'>('view')

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
  useEffect(() => setMode('view'), [id])

  const d = q.data
  const total = d?.artifact.current_version ?? 0
  const cur = d?.version ?? 0
  const isDesign = d?.kind === 'design' || d?.kind === 'html'
  const canDiff = !!d && TEXT_KINDS.has(d.kind) && cur > 1

  return (
    <aside className="fixed inset-0 z-20 md:static md:z-auto md:w-[46%] md:min-w-[380px] md:shrink-0 border-l border-line bg-bg-2 flex flex-col">
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
        {canDiff && (
          <button
            onClick={() => setMode(mode === 'diff' ? 'view' : 'diff')}
            className={clsx('p-1.5 rounded-md hover:bg-bg-3', mode === 'diff' ? 'text-fg bg-bg-3' : 'text-fg-2 hover:text-fg')}
            title={`Changes since v${cur - 1}`}
            aria-label="Show changes since the previous version"
            aria-pressed={mode === 'diff'}
          >
            <GitCompare size={16} />
          </button>
        )}
        {d && isDesign && (
          <button
            onClick={() => setMode(mode === 'variants' ? 'view' : 'variants')}
            className={clsx('p-1.5 rounded-md hover:bg-bg-3', mode === 'variants' ? 'text-fg bg-bg-3' : 'text-fg-2 hover:text-fg')}
            title="Generate variants"
            aria-label="Generate variants"
            aria-pressed={mode === 'variants'}
          >
            <Sparkles size={16} />
          </button>
        )}
        {d && (
          <a href={`/api/artifacts/${id}/export?version=${cur}`} download className="p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg" title="Download" aria-label="Download this version">
            <Download size={16} />
          </a>
        )}
        {d && (
          <a href={d.url} target="_blank" rel="noopener noreferrer" className="p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg" title="Open in new tab" aria-label="Open in new tab">
            <ExternalLink size={16} />
          </a>
        )}
        <button onClick={onClose} className="p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg" title="Close" aria-label="Close">
          <X size={16} />
        </button>
      </header>
      {d?.design_context && <DesignChips ctx={d.design_context} />}
      <div className="flex-1 min-h-0 relative bg-bg flex flex-col">
        {q.isLoading && <p className="meta p-4">Loading…</p>}
        {q.error && <p className="text-sm text-danger p-4">Could not load this artifact.</p>}
        {d && mode === 'diff' && <VersionDiff id={id} from={cur - 1} to={cur} toContent={d.content} />}
        {d && mode === 'variants' && <Variants id={id} version={cur} title={d.title} onDone={() => setMode('view')} />}
        {d && mode === 'view' && d.kind === 'code' && <pre className="h-full overflow-auto m-0 p-4 font-mono text-xs leading-relaxed">{d.content}</pre>}
        {d && mode === 'view' && d.kind !== 'code' && (
          <iframe
            ref={frame}
            key={d.version_id}
            title={d.title}
            src={d.url}
            sandbox="allow-scripts"
            referrerPolicy="no-referrer"
            className={clsx('w-full h-full border-0 bg-bg')}
          />
        )}
        {frameErr && mode === 'view' && (
          <div className="absolute bottom-0 inset-x-0 border-t border-danger/40 bg-bg-2 px-3 py-1.5 font-mono text-xs text-danger truncate" title={frameErr}>
            {frameErr}
          </div>
        )}
      </div>
    </aside>
  )
}

/** The design system a design version was made under: library and color swatches. */
function DesignChips({ ctx }: { ctx: DesignContext }) {
  const colors = Object.entries(ctx.colors ?? {})
  if (!ctx.library && colors.length === 0 && !ctx.radius && !ctx.spacing) return null
  return (
    <div className="shrink-0 border-b border-line px-3 py-1.5 flex items-center gap-3 overflow-x-auto text-xs text-fg-2">
      {ctx.library && <span>{ctx.library}</span>}
      {colors.map(([k, v]) => (
        <span key={k} className="flex items-center gap-1 shrink-0" title={`${k} ${v}`}>
          <span className="h-3 w-3 rounded-sm border border-line" style={{ background: v }} aria-hidden />
          {k}
        </span>
      ))}
      {ctx.radius && <span className="shrink-0">radius {ctx.radius}</span>}
      {ctx.spacing && <span className="shrink-0">spacing {ctx.spacing}</span>}
    </div>
  )
}

// ---- diff between two versions ----

type DiffRow = { t: ' ' | '+' | '-'; s: string }

/** Line diff by longest common subsequence; capped so a huge document stays responsive. */
export function lineDiff(a: string, b: string, cap = 3000): DiffRow[] {
  const A = a.split('\n').slice(0, cap)
  const B = b.split('\n').slice(0, cap)
  const n = A.length
  const m = B.length
  // dp[i][j] = LCS length of A[i:], B[j:], stored flat.
  const dp = new Uint32Array((n + 1) * (m + 1))
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i * (m + 1) + j] = A[i] === B[j] ? dp[(i + 1) * (m + 1) + j + 1] + 1 : Math.max(dp[(i + 1) * (m + 1) + j], dp[i * (m + 1) + j + 1])
    }
  }
  const out: DiffRow[] = []
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (A[i] === B[j]) {
      out.push({ t: ' ', s: A[i] })
      i++
      j++
    } else if (dp[(i + 1) * (m + 1) + j] >= dp[i * (m + 1) + j + 1]) {
      out.push({ t: '-', s: A[i] })
      i++
    } else {
      out.push({ t: '+', s: B[j] })
      j++
    }
  }
  for (; i < n; i++) out.push({ t: '-', s: A[i] })
  for (; j < m; j++) out.push({ t: '+', s: B[j] })
  return out
}

function VersionDiff({ id, from, to, toContent }: { id: string; from: number; to: number; toContent: string }) {
  const prev = useQuery({
    queryKey: ['artifact', id, from],
    queryFn: () => api.get<ArtifactDetail>(`/api/artifacts/${id}?version=${from}`),
  })
  const rows = useMemo(() => (prev.data ? lineDiff(prev.data.content, toContent) : []), [prev.data, toContent])
  const added = rows.filter((r) => r.t === '+').length
  const removed = rows.filter((r) => r.t === '-').length
  if (prev.isLoading) return <p className="meta p-4">Loading v{from}…</p>
  if (prev.error) return <p className="text-sm text-danger p-4">Could not load v{from}.</p>
  return (
    <div className="h-full flex flex-col min-h-0">
      <p className="shrink-0 px-4 py-2 meta border-b border-line-soft">
        v{from} → v{to}: {added} added, {removed} removed{rows.length >= 3000 ? ' (first 3000 lines)' : ''}
      </p>
      <pre className="flex-1 min-h-0 overflow-auto m-0 p-4 font-mono text-xs leading-relaxed">
        {rows.map((r, i) => (
          <div key={i} className={clsx('whitespace-pre', r.t === '+' && 'bg-bg-3 text-fg', r.t === '-' && 'text-danger line-through decoration-danger/40', r.t === ' ' && 'text-fg-3')}>
            <span className="inline-block w-4 select-none text-fg-4">{r.t === ' ' ? '' : r.t}</span>
            {r.s}
          </div>
        ))}
      </pre>
    </div>
  )
}

// ---- variants: N alternatives of this version ----

function Variants({ id, version, title, onDone }: { id: string; version: number; title: string; onDone: () => void }) {
  const open = useOpenArtifact()
  const [n, setN] = useState(3)
  const [instruction, setInstruction] = useState('')
  const [result, setResult] = useState<{ variants: ArtifactRef[]; errors: string[] } | null>(null)
  const gen = useMutation({
    mutationFn: () => api.post<{ variants: ArtifactRef[]; errors: string[] }>(`/api/artifacts/${id}/variants`, { n, version, instruction: instruction.trim() || undefined }),
    onSuccess: (r) => setResult(r),
  })
  return (
    <div className="h-full flex flex-col min-h-0">
      <div className="shrink-0 p-4 space-y-3 border-b border-line-soft">
        <p className="text-sm text-fg-2">
          Make alternatives of <span className="text-fg">{title}</span> v{version}. Each one is a model call and lands in this conversation as its own design, under the same design system.
        </p>
        <div className="flex gap-2">
          <input value={instruction} onChange={(e) => setInstruction(e.target.value)} placeholder="What should differ (optional): denser, darker, card layout…" className={inputSm} aria-label="What should differ" />
          <select value={n} onChange={(e) => setN(Number(e.target.value))} className={clsx(inputSm, 'w-20 shrink-0')} aria-label="How many">
            {[1, 2, 3, 4].map((k) => (
              <option key={k} value={k}>{k}</option>
            ))}
          </select>
          <button onClick={() => gen.mutate()} disabled={gen.isPending} className={btn.primarySm}>
            {gen.isPending ? 'Generating…' : 'Generate'}
          </button>
        </div>
        {gen.error && <Callout kind="error">{gen.error.message}</Callout>}
        {result && result.errors.length > 0 && <Callout kind="error">{result.errors.join(' · ')}</Callout>}
      </div>
      <div className="flex-1 min-h-0 overflow-y-auto p-4">
        {gen.isPending && <p className="meta">Generating {n} variant{n === 1 ? '' : 's'}…</p>}
        {result && result.variants.length > 0 && (
          <div className="grid grid-cols-2 gap-3">
            {result.variants.map((v) => (
              <button key={v.artifact_id} onClick={() => { open(v.artifact_id, v.version); onDone() }} className="text-left rounded-lg border border-line bg-bg-2 hover:bg-bg-3 overflow-hidden" title={`Open ${v.title}`}>
                <iframe title={v.title} src={v.url} sandbox="allow-scripts" referrerPolicy="no-referrer" className="w-full aspect-[4/3] border-0 bg-bg pointer-events-none" tabIndex={-1} />
                <span className="block truncate px-3 py-2 text-sm text-fg">{v.title}</span>
              </button>
            ))}
          </div>
        )}
        {!gen.isPending && !result && <p className="meta">Nothing generated yet.</p>}
      </div>
    </div>
  )
}
