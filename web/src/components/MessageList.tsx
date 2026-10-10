import { useEffect, useRef } from 'react'
import type { UIMessage } from 'ai'
import { Streamdown } from 'streamdown'
import clsx from 'clsx'
import { ChevronRight, ThumbsDown, ThumbsUp } from 'lucide-react'
import { Callout } from './ui'
import { ArtifactCard } from './ArtifactPanel'

type OnApproval = (approvalId: string, approved: boolean) => void
// A rating: 1, -1, or null to clear.
type OnRate = (messageId: string, score: 1 | -1 | null) => void

export function MessageList({
  messages,
  status,
  error,
  onApproval,
  ratings,
  onRate,
}: {
  messages: UIMessage[]
  status: string
  error?: Error
  onApproval?: OnApproval
  // The caller's ratings by message id, and the handler; absent means no
  // thumbs (the agent monitor, a transcript).
  ratings?: Record<string, number>
  onRate?: OnRate
}) {
  const bottom = useRef<HTMLDivElement>(null)
  useEffect(() => {
    bottom.current?.scrollIntoView({ block: 'end' })
  }, [messages, status])

  return (
    <div className="flex-1 min-h-0 overflow-y-auto">
      <div className="mx-auto max-w-3xl px-4 py-6 space-y-6">
        {messages.length === 0 && status === 'ready' && (
          <p className="tagline text-fg-3 text-center pt-24">Say something to begin.</p>
        )}
        {messages.map((m) => (
          <Message key={m.id} m={m} streaming={status === 'streaming' && m === messages[messages.length - 1]} onApproval={onApproval} rating={ratings?.[m.id]} onRate={onRate} />
        ))}
        {status === 'submitted' && <div className="thinking">Thinking…</div>}
        {error && <Callout kind="error">{error.message}</Callout>}
        <div ref={bottom} />
      </div>
    </div>
  )
}

/**
 * Where an answer came from. A message loaded from history carries it in
 * `metadata` (the server fills it from the stored row), but one streamed in
 * this session has no metadata: the stream announces it in a `data-model`
 * part at the start of each step instead. Read that when `metadata` is
 * absent, taking the last step's, as history does, so the notice shows
 * without a reload.
 */
function routeMeta(m: UIMessage): { model?: string; endpoint?: string } {
  const meta = (m.metadata ?? {}) as { model?: string; endpoint?: string }
  if (meta.endpoint) return meta
  for (let i = m.parts.length - 1; i >= 0; i--) {
    const p = m.parts[i] as { type: string; data?: { endpoint?: string; model?: string } }
    if (p.type === 'data-model' && p.data?.endpoint) return { model: p.data.model, endpoint: p.data.endpoint }
  }
  return meta
}

function Message({ m, streaming, onApproval, rating, onRate }: { m: UIMessage; streaming: boolean; onApproval?: OnApproval; rating?: number; onRate?: OnRate }) {
  const isUser = m.role === 'user'
  const meta = routeMeta(m)
  const canRate = !isUser && !streaming && !!onRate && m.parts.some((p) => p.type === 'text')
  return (
    <div className={clsx('flex reading', isUser ? 'justify-end' : 'justify-start')}>
      <div className={clsx('max-w-[85%]', isUser ? 'rounded-2xl bg-bg-3 px-4 py-2.5' : '')}>
        {m.parts.map((p, i) => {
          switch (p.type) {
            case 'text':
              return isUser ? (
                <p key={i} className="whitespace-pre-wrap">{p.text}</p>
              ) : (
                <div key={i} className="prose-ws">
                  <Streamdown>{p.text}</Streamdown>
                </div>
              )
            case 'reasoning':
              return <Reasoning key={i} text={p.text} open={streaming} />
            case 'file':
              return p.mediaType?.startsWith('image/') ? (
                <img key={i} src={p.url} alt={p.filename ?? ''} className="max-h-72 rounded-lg my-2" />
              ) : (
                <a key={i} href={p.url} className="text-accent border-b border-dotted border-link-underline hover:text-accent-hover text-sm">
                  {p.filename ?? 'file'}
                </a>
              )
            default:
              if (p.type === 'tool-create_artifact' || p.type === 'tool-update_artifact') {
                const tp = p as unknown as { output?: unknown; state: string }
                return <ArtifactCard key={i} output={tp.output} state={tp.state} />
              }
              if (p.type === 'tool-ask_user') {
                const tp = p as unknown as { input?: { question?: string } }
                return <QuestionCard key={i} question={tp.input?.question ?? ''} />
              }
              if (p.type === 'tool-generate_image' || p.type === 'tool-generate_video') {
                const tp = p as unknown as { input?: unknown; output?: unknown; state: string; errorText?: string }
                return <GeneratedMedia key={i} video={p.type === 'tool-generate_video'} input={tp.input} output={tp.output} state={tp.state} errorText={tp.errorText} />
              }
              if (p.type.startsWith('tool-') || p.type === 'dynamic-tool') {
                const tp = p as unknown as {
                  toolName?: string; type: string; input?: unknown; output?: unknown; state: string; errorText?: string
                  approval?: { id: string; approved?: boolean }
                }
                const name = tp.toolName ?? tp.type.replace(/^tool-/, '')
                if (tp.state === 'approval-requested' && tp.approval) {
                  return <ApprovalCard key={i} name={name} input={tp.input} approvalId={tp.approval.id} onApproval={onApproval} />
                }
                return <ToolCall key={i} name={name} state={tp.state} input={tp.input} output={tp.output} errorText={tp.errorText} />
              }
              return null
          }
        })}
        {!isUser && (meta.endpoint || canRate) && (
          <div className="meta mt-1 flex items-center gap-3">
            {meta.endpoint && <span>routed to {meta.endpoint}</span>}
            {canRate && (
              <span className="flex items-center gap-1 not-italic">
                <button
                  onClick={() => onRate!(m.id, rating === 1 ? null : 1)}
                  className={clsx('p-1 rounded-md hover:bg-bg-3', rating === 1 ? 'text-accent' : 'text-fg-4 hover:text-fg')}
                  title={rating === 1 ? 'Rated good; click to clear' : 'Good answer (a training signal)'}
                  aria-label="Good answer"
                  aria-pressed={rating === 1}
                >
                  <ThumbsUp size={13} />
                </button>
                <button
                  onClick={() => onRate!(m.id, rating === -1 ? null : -1)}
                  className={clsx('p-1 rounded-md hover:bg-bg-3', rating === -1 ? 'text-danger' : 'text-fg-4 hover:text-fg')}
                  title={rating === -1 ? 'Rated poor; click to clear' : 'Poor answer'}
                  aria-label="Poor answer"
                  aria-pressed={rating === -1}
                >
                  <ThumbsDown size={13} />
                </button>
              </span>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function Reasoning({ text, open }: { text: string; open: boolean }) {
  if (!text) return null
  return (
    <details open={open} className="my-2 font-sans text-sm text-fg-2 group">
      <summary className="cursor-pointer list-none flex items-center gap-1 select-none">
        <ChevronRight size={14} className="transition-transform group-open:rotate-90" /> Reasoning
      </summary>
      <div className="mt-1 pl-4 border-l border-dotted border-fg-3 whitespace-pre-wrap tagline text-fg-3">{text}</div>
    </details>
  )
}

function ToolCall({ name, state, input, output, errorText }: { name: string; state: string; input: unknown; output: unknown; errorText?: string }) {
  const label = state === 'output-error' ? 'failed' : state === 'output-available' ? 'done' : state.replace(/-/g, ' ')
  return (
    <details className="my-2 rounded-md border border-line font-sans text-sm">
      <summary className="cursor-pointer px-3 py-1.5 text-fg-2">
        {name} <span className={state === 'output-error' ? 'text-danger' : 'text-fg-3'}>· {label}</span>
      </summary>
      <pre className="px-3 py-2 overflow-x-auto font-mono text-xs">{JSON.stringify({ input, output, error: errorText }, null, 2)}</pre>
    </details>
  )
}

/** generate_image and generate_video: the files inline, the tool's JSON only when it failed. */
function GeneratedMedia({ video, input, output, state, errorText }: { video: boolean; input: unknown; output: unknown; state: string; errorText?: string }) {
  type File = { url: string; width?: number; height?: number; mime?: string }
  const out = (output ?? {}) as { images?: File[]; videos?: File[]; kind?: string; endpoint?: string; cost_usd?: number }
  const files = video ? out.videos : out.images
  const inp = (input ?? {}) as { prompt?: string; source_attachment_id?: string }
  const name = video ? 'generate_video' : 'generate_image'
  if (state === 'output-error' || (state === 'output-available' && !files?.length)) {
    return <ToolCall name={name} state={state} input={input} output={output} errorText={errorText} />
  }
  if (state !== 'output-available') {
    return (
      <div className="my-2 font-sans text-sm text-fg-2">
        <span className="thinking">{video ? (inp.source_attachment_id ? 'Animating the image…' : 'Rendering a video…') : inp.source_attachment_id ? 'Editing the image…' : 'Generating an image…'}</span>
        {inp.prompt && <div className="meta mt-1 truncate">{inp.prompt}</div>}
        {video && <div className="meta mt-1">Video takes a few minutes; it also lands in the project's gallery.</div>}
      </div>
    )
  }
  return (
    <div className="my-2">
      <div className="flex flex-wrap gap-2">
        {files!.map((f) =>
          video ? (
            <video key={f.url} src={f.url} controls playsInline preload="metadata" width={f.width ?? undefined} height={f.height ?? undefined} className="max-h-80 w-auto rounded-lg border border-line bg-bg-2" />
          ) : (
            <a key={f.url} href={f.url} target="_blank" rel="noopener">
              <img src={f.url} alt={inp.prompt ?? 'generated image'} width={f.width ?? undefined} height={f.height ?? undefined} className="max-h-80 w-auto rounded-lg border border-line" />
            </a>
          ),
        )}
      </div>
      <div className="meta mt-1">
        {out.kind === 'edit' ? 'edited' : 'made'}
        {out.endpoint ? ` with ${out.endpoint}` : ''}
        {out.cost_usd ? ` · $${out.cost_usd.toFixed(3)}` : ''}
      </div>
    </div>
  )
}

/** ask_user: the model stopped to ask something. The turn is over; the reply goes in the composer. */
function QuestionCard({ question }: { question: string }) {
  return (
    <div role="note" aria-label="Question from the assistant" className="my-2 rounded-md border border-line bg-bg-2 px-3 py-2">
      <div className="meta">A question for you</div>
      <p className="reading-tight mt-1 whitespace-pre-wrap">{question}</p>
    </div>
  )
}

/** A tool call waiting for the user. Approve or decline; the run resumes on its own. */
function ApprovalCard({ name, input, approvalId, onApproval }: { name: string; input: unknown; approvalId: string; onApproval?: OnApproval }) {
  return (
    <div className="my-2 rounded-lg border border-accent/50 bg-bg-2 font-sans text-sm">
      <div className="px-3 py-2">
        <div className="text-fg">The assistant wants to run <span className="font-mono text-xs">{name}</span>.</div>
        <pre className="mt-1 max-h-40 overflow-auto font-mono text-xs text-fg-2">{JSON.stringify(input, null, 2)}</pre>
      </div>
      <div className="flex gap-2 border-t border-line px-3 py-2">
        <button onClick={() => onApproval?.(approvalId, true)} className="rounded-lg bg-accent text-accent-fg px-4 py-1.5 text-sm font-medium hover:bg-accent-strong">Allow</button>
        <button onClick={() => onApproval?.(approvalId, false)} className="rounded-lg border border-line px-4 py-1.5 text-sm hover:bg-bg-3">Decline</button>
      </div>
    </div>
  )
}
