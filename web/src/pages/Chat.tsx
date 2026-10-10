import { useEffect, useMemo, useRef, useState } from 'react'
import { useParams, useSearchParams } from 'react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useChat } from '@ai-sdk/react'
import { DefaultChatTransport, lastAssistantMessageIsCompleteWithApprovalResponses, type UIMessage } from 'ai'
import { api, type Conversation, type ModelsResponse } from '../api'
import { MessageList } from '../components/MessageList'
import { Composer, type FilePart } from '../components/Composer'
import { ModelPicker } from '../components/ModelPicker'
import { ArtifactContext, ArtifactPanel, type ArtifactRef } from '../components/ArtifactPanel'
import { CodePanel } from '../components/CodePanel'
import { DesignPanel } from '../components/DesignPanel'
import { Palette, PanelRight } from 'lucide-react'

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
        qc.invalidateQueries({ queryKey: ['convs'] })
        // The cached conversation seeds the chat when this page is opened
        // again; without this, going back to it shows the empty copy loaded
        // when it was created, until a reload.
        qc.invalidateQueries({ queryKey: ['conv', id] })
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

  // Artifact panel: open the newest artifact when one arrives.
  const [panel, setPanel] = useState<{ id: string; version?: number } | null>(() => {
    const a = latestArtifact(initial.messages)
    return a ? { id: a.artifact_id, version: a.version } : null
  })
  const lastSeen = useRef<string | null>(latestArtifact(initial.messages)?.version_id ?? null)
  useEffect(() => {
    const a = latestArtifact(chat.messages)
    if (a && a.version_id !== lastSeen.current) {
      lastSeen.current = a.version_id
      setPanel({ id: a.artifact_id, version: a.version })
    }
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
  const isCode = initial.conversation.mode === 'code'
  const isDesign = initial.conversation.mode === 'design'
  const [codeOpen, setCodeOpen] = useState(isCode)
  const [designOpen, setDesignOpen] = useState(false)

  return (
    <ArtifactContext.Provider value={{ open: (aid, version) => setPanel({ id: aid, version }) }}>
      <div className="flex-1 min-h-0 flex">
        <div className="flex-1 min-w-0 flex flex-col">
          <header className="h-12 shrink-0 border-b border-line flex items-center justify-between px-4 gap-3">
            <h2 className="reading-tight truncate text-fg">{initial.conversation.title || 'New conversation'}</h2>
            <div className="flex items-center gap-2">
              <ModelPicker value={model} onChange={setModel} models={models} reasoning={reasoning} onReasoning={setReasoning} />
              {isCode && !codeOpen && (
                <button onClick={() => setCodeOpen(true)} className="p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg" title="Files, terminal, preview" aria-label="Open code panel">
                  <PanelRight size={16} />
                </button>
              )}
              {isDesign && (
                <button
                  onClick={() => setDesignOpen(!designOpen)}
                  className={designOpen ? 'p-1.5 rounded-md text-fg bg-bg-3' : 'p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg'}
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
            onApproval={(approvalId, approved) => chat.addToolApprovalResponse({ id: approvalId, approved })}
            ratings={ratings.data?.ratings}
            onRate={(messageId, score) => rate.mutate({ messageId, score })}
          />
          <Composer
            disabled={busy}
            imageNote={noVisionNote(model, models)}
            initialFiles={initialFiles}
            onStop={busy ? () => chat.stop() : undefined}
            onSend={(text, files) => {
              chat.sendMessage({ text, files })
            }}
          />
        </div>
        {isCode && codeOpen && <CodePanel projectId={initial.conversation.project_id} onClose={() => setCodeOpen(false)} />}
        {isDesign && designOpen && <DesignPanel conversationId={id} settings={initial.conversation.settings} onClose={() => setDesignOpen(false)} />}
        {panel && !(isCode && codeOpen) && !designOpen && (
          <ArtifactPanel
            id={panel.id}
            version={panel.version}
            onClose={() => setPanel(null)}
            onVersion={(v) => setPanel({ id: panel.id, version: v })}
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
