import { useEffect, useRef } from 'react'
import type { UIMessage } from 'ai'
import { Streamdown } from 'streamdown'
import clsx from 'clsx'
import { ChevronRight } from 'lucide-react'
import { Callout } from './ui'
import { ArtifactCard } from './ArtifactPanel'

type OnApproval = (approvalId: string, approved: boolean) => void

export function MessageList({
  messages,
  status,
  error,
  onApproval,
}: {
  messages: UIMessage[]
  status: string
  error?: Error
  onApproval?: OnApproval
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
          <Message key={m.id} m={m} streaming={status === 'streaming' && m === messages[messages.length - 1]} onApproval={onApproval} />
        ))}
        {status === 'submitted' && <div className="thinking">Thinking…</div>}
        {error && <Callout kind="error">{error.message}</Callout>}
        <div ref={bottom} />
      </div>
    </div>
  )
}

function Message({ m, streaming, onApproval }: { m: UIMessage; streaming: boolean; onApproval?: OnApproval }) {
  const isUser = m.role === 'user'
  const meta = (m.metadata ?? {}) as { model?: string; endpoint?: string }
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
              if (p.type === 'tool-generate_image') {
                const tp = p as unknown as { input?: unknown; output?: unknown; state: string; errorText?: string }
                return <GeneratedImages key={i} input={tp.input} output={tp.output} state={tp.state} errorText={tp.errorText} />
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
        {!isUser && meta.endpoint && <div className="meta mt-1">routed to {meta.endpoint}</div>}
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

/** generate_image: the images inline, the tool's JSON only when it failed. */
function GeneratedImages({ input, output, state, errorText }: { input: unknown; output: unknown; state: string; errorText?: string }) {
  const out = (output ?? {}) as { images?: { url: string; width?: number; height?: number }[]; endpoint?: string; cost_usd?: number }
  const prompt = ((input ?? {}) as { prompt?: string }).prompt
  if (state === 'output-error' || (state === 'output-available' && !out.images?.length)) {
    return <ToolCall name="generate_image" state={state} input={input} output={output} errorText={errorText} />
  }
  if (state !== 'output-available') {
    return (
      <div className="my-2 font-sans text-sm text-fg-2">
        <span className="thinking">Generating an image…</span>
        {prompt && <div className="meta mt-1 truncate">{prompt}</div>}
      </div>
    )
  }
  return (
    <div className="my-2">
      <div className="flex flex-wrap gap-2">
        {out.images!.map((im) => (
          <a key={im.url} href={im.url} target="_blank" rel="noopener">
            <img src={im.url} alt={prompt ?? 'generated image'} width={im.width ?? undefined} height={im.height ?? undefined} className="max-h-80 w-auto rounded-lg border border-line" />
          </a>
        ))}
      </div>
      <div className="meta mt-1">
        {out.endpoint ? `made with ${out.endpoint}` : 'generated'}
        {out.cost_usd ? ` · $${out.cost_usd.toFixed(3)}` : ''}
      </div>
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
