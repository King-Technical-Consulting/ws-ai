import { useEffect, useRef } from 'react'
import type { UIMessage } from 'ai'
import { Streamdown } from 'streamdown'
import clsx from 'clsx'
import { ChevronRight, ThumbsDown, ThumbsUp } from 'lucide-react'
import { Callout, StarButton } from './ui'
import { ArtifactCard } from './ArtifactPanel'
import { PlanCard, stepMarks, type PlanInput } from './PlanCard'
import { Link } from 'react-router'
import { parseRichHref, rewriteRichLinks } from '../lib/richlinks'

// reason carries an edited plan, or why a plan was rejected (plan mode).
type OnApproval = (approvalId: string, approved: boolean, reason?: string) => void
// A rating: 1, -1, or null to clear.
type OnRate = (messageId: string, score: 1 | -1 | null) => void

export function MessageList({
  messages,
  status,
  error,
  onApproval,
  ratings,
  onRate,
  notebookId,
  isStarred,
  onStar,
}: {
  // Favorites (PLAN M13 cut 7): whether a message is starred, and the toggle.
  isStarred?: (messageId: string) => boolean
  onStar?: (messageId: string) => void
  // The research notebook this chat belongs to, so a source link in a reply
  // can jump to it (PLAN M13).
  notebookId?: string
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
          <Message key={m.id} m={m} streaming={status === 'streaming' && m === messages[messages.length - 1]} onApproval={onApproval} rating={ratings?.[m.id]} onRate={onRate} notebookId={notebookId} starred={isStarred?.(m.id)} onStar={onStar ? () => onStar(m.id) : undefined} />
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

function Message({ m, streaming, onApproval, rating, onRate, notebookId, starred, onStar }: { m: UIMessage; streaming: boolean; onApproval?: OnApproval; rating?: number; onRate?: OnRate; notebookId?: string; starred?: boolean; onStar?: () => void }) {
  const isUser = m.role === 'user'
  const meta = routeMeta(m)
  const canRate = !isUser && !streaming && !!onRate && m.parts.some((p) => p.type === 'text')
  const canStar = !isUser && !streaming && !!onStar && m.parts.some((p) => p.type === 'text')
  return (
    <div className={clsx('flex reading', isUser ? 'justify-end' : 'justify-start')}>
      <div className={clsx('max-w-[85%]', isUser ? 'rounded-2xl bg-bg-3 px-4 py-2.5' : '')}>
        {m.parts.map((p, i) => {
          switch (p.type) {
            case 'text': {
              // An uploaded document's extracted text (server side, PLAN M13)
              // arrives as a text part with a header line; show it as a chip.
              const doc = isUser ? documentHeader(p.text) : null
              if (doc) return <DocumentChip key={i} head={doc.head} body={doc.body} />
              return isUser ? (
                <p key={i} className="whitespace-pre-wrap">{p.text}</p>
              ) : (
                <div key={i} className="prose-ws">
                  <Streamdown components={{ a: (props) => <RichAnchor {...props} notebookId={notebookId} /> }}>{rewriteRichLinks(p.text)}</Streamdown>
                </div>
              )
            }
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
              if (p.type === 'data-memory') {
                const refs = ((p as unknown as { data?: { id: string; kind: string; content: string }[] }).data ?? [])
                if (!refs.length) return null
                return (
                  <details key={i} className="my-2 font-sans text-sm text-fg-2">
                    <summary className="cursor-pointer meta">from memory · {refs.length}</summary>
                    <ul className="mt-1 pl-4 space-y-0.5">
                      {refs.map((r) => (
                        <li key={r.id} className="reading-tight">{r.content}</li>
                      ))}
                    </ul>
                  </details>
                )
              }
              if (p.type === 'tool-plan_step') return null // ticks the plan's checklist; shown there
              if (p.type === 'tool-submit_plan') {
                const tp = p as unknown as { input?: PlanInput; output?: unknown; state: string; approval?: { id: string } }
                if (!tp.input?.steps) return null
                return (
                  <PlanCard key={i} input={tp.input} state={tp.state} output={tp.output} approvalId={tp.approval?.id} marks={stepMarks(m.parts as { type: string; input?: unknown }[])} onDecision={onApproval} />
                )
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
        {!isUser && (meta.endpoint || canRate || canStar) && (
          <div className="meta mt-1 flex items-center gap-3">
            {meta.endpoint && <span>routed to {meta.endpoint}</span>}
            {canStar && <StarButton starred={!!starred} onToggle={onStar!} label="Favorite this answer" />}
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

const DOC_HEADER = /^\[Attached document: ([^\]]*)\]\n?/

/** The header and body of an uploaded document's text part, or null for any other text. */
export function documentHeader(text: string): { head: string; body: string } | null {
  const m = DOC_HEADER.exec(text)
  return m ? { head: m[1], body: text.slice(m[0].length) } : null
}

/** An attached document: its name and size on one line, the extracted text behind a disclosure. */
function DocumentChip({ head, body }: { head: string; body: string }) {
  // "name · pdf · 12 page(s) · text layer · attachment <id>": show the first three.
  const bits = head.split(' · ')
  const label = bits.slice(0, 3).join(' · ')
  return (
    <details className="my-1 rounded-md border border-line bg-bg-2 font-sans text-sm" data-testid="document-chip">
      <summary className="cursor-pointer px-3 py-1.5 text-fg-2">{label}</summary>
      <pre className="max-h-64 overflow-auto whitespace-pre-wrap px-3 py-2 font-mono text-xs">{body}</pre>
    </details>
  )
}

/** A link in a reply: a tooltip for tip:, a jump to the notebook source for src:, else an ordinary link. */
function RichAnchor({ href, children, notebookId, ...rest }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { notebookId?: string; node?: unknown }) {
  const rich = parseRichHref(href)
  if (rich?.kind === 'tip') {
    return (
      <span className="relative group inline-block">
        <span tabIndex={0} className="border-b border-dotted border-fg-3 cursor-help" data-testid="rich-tip">
          {children}
        </span>
        <span
          role="tooltip"
          className="hidden group-hover:block group-focus-within:block absolute left-0 top-full z-10 mt-1 w-64 rounded-md border border-line bg-bg-2 px-3 py-2 font-sans text-sm text-fg"
        >
          {rich.text}
        </span>
      </span>
    )
  }
  if (rich?.kind === 'src') {
    const to = notebookId ? `/notebooks/${notebookId}#source-${rich.id}${rich.page ? `-p${rich.page}` : ''}` : null
    const label = (
      <>
        {children}
        {rich.page ? <sup className="ml-0.5 text-[11px]">p.{rich.page}</sup> : null}
      </>
    )
    return to ? (
      <Link to={to} className="text-accent border-b border-dotted border-link-underline hover:text-accent-hover" data-testid="rich-src">
        {label}
      </Link>
    ) : (
      <span className="border-b border-dotted border-fg-3" data-testid="rich-src">{label}</span>
    )
  }
  const { node: _node, ...anchor } = rest as { node?: unknown } & React.AnchorHTMLAttributes<HTMLAnchorElement>
  void _node
  return (
    <a href={href} {...anchor} className="text-accent border-b border-dotted border-link-underline hover:text-accent-hover" target="_blank" rel="noreferrer noopener">
      {children}
    </a>
  )
}
