import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router'
import { useFavorites } from '../lib/favorites'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useChat } from '@ai-sdk/react'
import { DefaultChatTransport, lastAssistantMessageIsCompleteWithApprovalResponses, type UIMessage } from 'ai'
import { api, type Conversation, type ModelsResponse } from '../api'
import { MessageList } from '../components/MessageList'
import { Composer, type FilePart } from '../components/Composer'
import { ModelPicker } from '../components/ModelPicker'
import { ArtifactContext, ArtifactPanel, ArtifactsList, type ArtifactRef } from '../components/ArtifactPanel'
import { CodePanel } from '../components/CodePanel'
import { DesignPanel } from '../components/DesignPanel'
import { Palette, PanelRight, SquareTerminal } from 'lucide-react'
import clsx from 'clsx'

type ConvResponse = { conversation: Conversation; messages: UIMessage[] }

export default function Chat() {
  const { id = '' } = useParams()
  const qc = useQueryClient()
  const conv = useQuery({ queryKey: ['conv', id], queryFn: () => api.get<ConvResponse>(`/api/conversations/${id}`) })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models'), staleTime: 60_000 })

  if (conv.isLoading) return <div className="p-6 meta">Loading…</div>
  if (conv.error || !conv.data) return <div className="p-6 text-danger text-sm">Could not load this conversation.</div>

  return (
    <ChatInner
      key={id}
      id={id}
      initial={conv.data}
      models={models.data}
      onTitleMaybeChanged={() => {
        const refresh = () => {
          qc.invalidateQueries({ queryKey: ['convs'] })
          // The cached conversation seeds the chat when this page is opened
          // again; without this, going back to it shows the empty copy loaded
          // when it was created, until a reload.
          qc.invalidateQueries({ queryKey: ['conv', id] })
        }
        refresh()
        // The title is written by a background job after the reply ends, a
        // model call away, so read again while it lands. A refresh after
        // leaving the page is harmless: the client is shared.
        for (const ms of [3000, 8000, 20000]) setTimeout(refresh, ms)
      }}
    />
  )
}

/** Finds the newest artifact output across all messages. */
function latestArtifact(messages: UIMessage[]): ArtifactRef | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const parts = messages[i].parts
    for (let j = parts.length - 1; j >= 0; j--) {
      const p = parts[j] as unknown as { type: string; state?: string; output?: ArtifactRef }
      if ((p.type === 'tool-create_artifact' || p.type === 'tool-update_artifact') && p.state === 'output-available' && p.output?.artifact_id) {
        return p.output
      }
    }
  }
  return null
}

function ChatInner({
  id,
  initial,
  models,
  onTitleMaybeChanged,
}: {
  id: string
  initial: ConvResponse
  models?: ModelsResponse
  onTitleMaybeChanged: () => void
}) {
  const [model, setModel] = useState(initial.conversation.model_selector || 'auto')
  const [reasoning, setReasoning] = useState(false)
  const [plan, setPlan] = useState(false)
  const modelRef = useRef(model)
  const reasoningRef = useRef(reasoning)
  modelRef.current = model
  reasoningRef.current = reasoning

  const transport = useMemo(
    () =>
      new DefaultChatTransport({
        api: `/api/conversations/${id}/chat`,
        credentials: 'same-origin',
        body: () => ({ model: modelRef.current, reasoning: reasoningRef.current }),
      }),
    [id],
  )

  const chat = useChat({
    id,
    transport,
    messages: initial.messages,
    // After the user answers every pending approval, re-send so the server
    // resumes the paused run and streams the rest of the turn.
    sendAutomaticallyWhen: lastAssistantMessageIsCompleteWithApprovalResponses,
    onFinish: () => onTitleMaybeChanged(),
  })

  useEffect(() => {
    if (model !== initial.conversation.model_selector) {
      api.patch(`/api/conversations/${id}`, { model }).catch(() => {})
    }
  }, [model, id, initial.conversation.model_selector])

  // Artifact panel: closed when a conversation is opened, whatever it holds. An artifact that
  // arrives while you are here opens it, on a screen wide enough to show it beside the chat; on a
  // phone the panel covers the chat, so it only opens when you tap the artifact's card.
  const [panel, setPanel] = useState<{ id: string; version?: number } | null>(null)
  // One right-hand panel at a time: the sandbox, the design system, the artifacts list or one
  // artifact. Opening one closes the others.
  const isCode = initial.conversation.mode === 'code'
  const isDesign = initial.conversation.mode === 'design'
  const [codeOpen, setCodeOpen] = useState(isCode)
  const [designOpen, setDesignOpen] = useState(false)
  const [listOpen, setListOpen] = useState(false)
  const showOnly = (which: 'code' | 'design' | 'list' | 'artifact' | null, artifact?: { id: string; version?: number }) => {
    setCodeOpen(which === 'code')
    setDesignOpen(which === 'design')
    setListOpen(which === 'list')
    setPanel(which === 'artifact' && artifact ? artifact : null)
  }
  const lastSeen = useRef<string | null>(latestArtifact(initial.messages)?.version_id ?? null)
  const anyOpen = useRef(false)
  anyOpen.current = codeOpen || designOpen || listOpen
  useEffect(() => {
    const a = latestArtifact(chat.messages)
    if (a && a.version_id !== lastSeen.current) {
      lastSeen.current = a.version_id
      qc.invalidateQueries({ queryKey: ['conv-artifacts', id] })
      // Beside the chat, never over it, and never in place of a panel that is open.
      if (window.matchMedia('(min-width: 768px)').matches && !anyOpen.current) setPanel({ id: a.artifact_id, version: a.version })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chat.messages])

  // Ratings (PLAN M10): thumbs on assistant answers, one per user.
  const qc = useQueryClient()
  const ratings = useQuery({ queryKey: ['ratings', id], queryFn: () => api.get<{ ratings: Record<string, number> }>(`/api/conversations/${id}/ratings`) })
  const rate = useMutation({
    mutationFn: ({ messageId, score }: { messageId: string; score: 1 | -1 | null }) =>
      score === null ? api.del(`/api/messages/${messageId}/rating`) : api.put(`/api/messages/${messageId}/rating`, { score }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['ratings', id] }),
  })

  // ?attach=<attachment id> (the gallery's "use in chat"): load the image
  // into the composer as if the user had attached it, then drop the param.
  const [params, setParams] = useSearchParams()
  const attach = params.get('attach')
  const [initialFiles, setInitialFiles] = useState<FilePart[] | undefined>(undefined)
  useEffect(() => {
    if (!attach) return
    let cancelled = false
    ;(async () => {
      try {
        const res = await fetch(`/api/attachments/${attach}`, { credentials: 'same-origin' })
        if (!res.ok) return
        const blob = await res.blob()
        const url = await new Promise<string>((ok) => {
          const r = new FileReader()
          r.onload = () => ok(r.result as string)
          r.readAsDataURL(blob)
        })
        if (!cancelled) setInitialFiles([{ type: 'file', mediaType: blob.type || 'image/png', url, filename: 'image' }])
      } finally {
        if (!cancelled) setParams({}, { replace: true })
      }
    })()
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [attach])

  const busy = chat.status === 'submitted' || chat.status === 'streaming'
  const fav = useFavorites()
  const nav = useNavigate()
  const prompts = useQuery({ queryKey: ['saved-prompts'], queryFn: () => api.get<{ prompts: { id: string; name: string; text: string }[] }>('/api/saved-prompts') })
  // A research notebook's chat carries the notebook id in its settings.
  const notebookId = (initial.conversation.settings as { notebook_id?: string } | null | undefined)?.notebook_id
  // Whether there is an artifact to list: one in the messages, or any the server has for this
  // conversation (a code conversation's artifacts come from elsewhere than its chat).
  const artifactRows = useQuery({ queryKey: ['conv-artifacts', id], queryFn: () => api.get<unknown[] | null>(`/api/conversations/${id}/artifacts`) })
  const hasArtifacts = latestArtifact(chat.messages) !== null || (artifactRows.data?.length ?? 0) > 0
  const headerBtn = (on: boolean) => clsx('shrink-0 p-1.5 min-h-9 min-w-9 md:min-h-0 md:min-w-0 inline-flex items-center justify-center rounded-md', on ? 'text-fg bg-bg-3' : 'text-fg-2 hover:bg-bg-3 hover:text-fg')

  return (
    <ArtifactContext.Provider value={{ open: (aid, version) => showOnly('artifact', { id: aid, version }) }}>
      <div className="flex-1 min-h-0 flex">
        <div className="flex-1 min-w-0 flex flex-col">
          <header className="h-12 shrink-0 border-b border-line flex items-center justify-between px-4 gap-3">
            <h2 className="reading-tight truncate text-fg">{initial.conversation.title || 'New conversation'}</h2>
            {notebookId && (
              <Link to={`/notebooks/${notebookId}`} className="meta shrink-0 text-accent border-b border-dotted border-link-underline hover:text-accent-hover">
                Notebook
              </Link>
            )}
            {/* min-w-0 and shrink-0: with a side panel open the chat column is narrow, and it is the model picker that gives way, not the panel buttons */}
            <div className="flex items-center gap-2 min-w-0">
              <ModelPicker value={model} onChange={setModel} models={models} reasoning={reasoning} onReasoning={setReasoning} />
              {hasArtifacts && (
                <button
                  onClick={() => showOnly(listOpen || panel ? null : 'list')}
                  className={headerBtn(listOpen || !!panel)}
                  title="Artifacts"
                  aria-label="Artifacts"
                  aria-pressed={listOpen || !!panel}
                >
                  <PanelRight size={16} />
                </button>
              )}
              {isCode && (
                <button
                  onClick={() => showOnly(codeOpen ? null : 'code')}
                  className={headerBtn(codeOpen)}
                  title="Sandbox: files, terminal, preview"
                  aria-label="Sandbox: files, terminal, preview"
                  aria-pressed={codeOpen}
                >
                  <SquareTerminal size={16} />
                </button>
              )}
              {isDesign && (
                <button
                  onClick={() => showOnly(designOpen ? null : 'design')}
                  className={headerBtn(designOpen)}
                  title="Design system"
                  aria-label="Design system"
                  aria-pressed={designOpen}
                >
                  <Palette size={16} />
                </button>
              )}
            </div>
          </header>
          <MessageList
            messages={chat.messages}
            status={chat.status}
            error={chat.error}
            onApproval={(approvalId, approved, reason) => chat.addToolApprovalResponse({ id: approvalId, approved, reason })}
            ratings={ratings.data?.ratings}
            onRate={(messageId, score) => rate.mutate({ messageId, score })}
            notebookId={notebookId}
            isStarred={(mid) => fav.isStarred('message', mid)}
            onStar={(mid) => fav.toggle('message', mid)}
          />
          <Composer
            disabled={busy}
            plan={plan}
            onPlan={setPlan}
            savedPrompts={prompts.data?.prompts}
            onCommand={async (name, arg) => {
              if (name === 'remember') {
                if (!arg) return 'Say what to remember: /remember I prefer metric units.'
                try {
                  await api.post('/api/memory', { content: arg })
                  return 'Remembered. You can change it under Settings, Memory.'
                } catch (e) {
                  return e instanceof Error ? e.message : 'Could not remember that.'
                }
              }
              if (name === 'schedule') nav(arg ? `/scheduled?prompt=${encodeURIComponent(arg)}` : '/scheduled')
              if (name === 'notebook') nav('/notebooks')
            }}
            imageNote={noVisionNote(model, models)}
            initialFiles={initialFiles}
            onStop={busy ? () => chat.stop() : undefined}
            onSend={(text, files, opts) => {
              // Plan mode applies to this message only; the resume that
              // follows an approval is not a new turn and never carries it.
              chat.sendMessage({ text, files }, opts?.plan ? { body: { plan: true } } : undefined)
              setPlan(false)
            }}
          />
        </div>
        {isCode && codeOpen && <CodePanel projectId={initial.conversation.project_id} onClose={() => setCodeOpen(false)} />}
        {isDesign && designOpen && <DesignPanel conversationId={id} settings={initial.conversation.settings} onClose={() => setDesignOpen(false)} />}
        {listOpen && <ArtifactsList conversationId={id} onPick={(aid, version) => showOnly('artifact', { id: aid, version })} onClose={() => setListOpen(false)} />}
        {panel && (
          <ArtifactPanel
            id={panel.id}
            version={panel.version}
            onClose={() => setPanel(null)}
            onVersion={(v) => setPanel({ id: panel.id, version: v })}
            onBack={() => showOnly('list')}
          />
        )}
      </div>
    </ArtifactContext.Provider>
  )
}

/**
 * Why an attached image would fail with this model choice, or undefined
 * when it would not: a direct pick without vision, or an alias none of
 * whose listed models has it (the router then refuses the message).
 */
function noVisionNote(selector: string, models?: ModelsResponse): string | undefined {
  if (!models) return undefined
  const direct = models.models.find((m) => m.id === selector)
  if (direct) return direct.capabilities.vision ? undefined : `${direct.display_name} can't read images; pick a model that can.`
  const ids = models.aliases[selector]
  if (!ids || ids.length === 0) return undefined
  const listed = models.models.filter((m) => ids.includes(m.id))
  if (listed.length === 0 || listed.some((m) => m.capabilities.vision)) return undefined
  return `No model under "${selector}" can read images; pick one that can.`
}
