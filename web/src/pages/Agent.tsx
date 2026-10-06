import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { UIMessage } from 'ai'
import clsx from 'clsx'
import { api, type AgentDetail, type AgentInput, type AgentMemory, type AgentRun, type AgentStep, type AgentTrigger, type Approval, type Conversation, type ModelsResponse, type Project } from '../api'
import { SectionHead, PageTitle, Callout, ListGroup, btn, inputSm } from '../components/ui'
import { MessageList } from '../components/MessageList'
import { AgentForm, ago, until } from './Agents'
import { useMe } from '../App'

const OPEN = new Set(['queued', 'running', 'paused_approval', 'paused_steer'])

/**
 * Agent monitor (PLAN M7): one agent's triggers, runs, approvals inbox,
 * spend against its budget, and a run view with the step timeline, the
 * transcript and a steer box. Polls while a run is open.
 */
export default function AgentMonitor() {
  const { id = '' } = useParams()
  const qc = useQueryClient()
  const nav = useNavigate()
  const detail = useQuery({
    queryKey: ['agent', id],
    queryFn: () => api.get<AgentDetail>(`/api/agents/${id}`),
    refetchInterval: (q) => (q.state.data?.runs.some((r) => OPEN.has(r.status)) ? 3000 : 15000),
  })
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models'), staleTime: 60_000 })
  const me = useMe()
  const [editing, setEditing] = useState(false)
  const [selectedRun, setSelectedRun] = useState<string | null>(null)
  const refresh = () => qc.invalidateQueries({ queryKey: ['agent', id] })

  const update = useMutation({
    mutationFn: (input: AgentInput) => api.put(`/api/agents/${id}`, input),
    onSuccess: () => {
      setEditing(false)
      refresh()
    },
  })
  const setEnabled = useMutation({
    mutationFn: (enabled: boolean) => api.post(`/api/agents/${id}/enabled`, { enabled }),
    onSuccess: refresh,
  })
  const runNow = useMutation({
    mutationFn: () => api.post<AgentRun>(`/api/agents/${id}/run`, {}),
    onSuccess: (r) => {
      setSelectedRun(r.id)
      refresh()
    },
  })
  const remove = useMutation({
    mutationFn: () => api.del(`/api/agents/${id}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['agents'] })
      nav('/agents')
    },
  })

  if (detail.isLoading) return <div className="p-6 meta">Loading…</div>
  if (detail.error || !detail.data) return <div className="p-6"><Callout kind="error">{(detail.error as Error)?.message ?? 'Could not load this agent.'}</Callout></div>
  const d = detail.data
  const a = d.agent
  const run = d.runs.find((r) => r.id === selectedRun) ?? d.runs[0]
  const budget = d.spend.budgets.find((b) => b.period === 'month') ?? d.spend.budgets[0]

  return (
    <div className="flex-1 min-h-0 flex">
      <div className="w-[28rem] shrink-0 border-r border-line overflow-y-auto">
        <div className="p-6 space-y-6">
          <div className="space-y-1">
            <Link to="/agents" className="meta">← agents</Link>
            <div className="flex items-baseline justify-between gap-3">
              <PageTitle>{a.name}</PageTitle>
              <span className="section-head border-0 shrink-0">{a.enabled ? 'enabled' : 'paused'}</span>
            </div>
            {a.goal && <p className="reading text-fg-2">{a.goal}</p>}
            <p className="meta">
              {a.model_policy?.selector ?? 'auto'} · {a.model_policy?.task_class ?? 'chat'} · up to {a.max_steps} steps
              {a.last_run_at ? ` · last run ${ago(a.last_run_at)}` : ''}
            </p>
          </div>

          <div className="flex flex-wrap gap-2">
            <button onClick={() => runNow.mutate()} disabled={runNow.isPending || !a.enabled} className={btn.primarySm}>
              Run now
            </button>
            <button onClick={() => setEnabled.mutate(!a.enabled)} disabled={setEnabled.isPending} className={btn.secondarySm}>
              {a.enabled ? 'Pause' : 'Resume'}
            </button>
            <button onClick={() => setEditing((v) => !v)} className={btn.secondarySm}>
              {editing ? 'Close' : 'Edit'}
            </button>
            <button
              onClick={() => {
                if (confirm(`Delete ${a.name} and its runs?`)) remove.mutate()
              }}
              className={clsx(btn.danger, 'ml-auto')}
            >
              delete
            </button>
          </div>
          {runNow.error && <Callout kind="error">{(runNow.error as Error).message}</Callout>}

          {editing && (
            <AgentForm initial={a} projects={projects.data ?? []} models={models.data} pending={update.isPending} error={update.error as Error | null} onCancel={() => setEditing(false)} onSubmit={(input) => update.mutate(input)} />
          )}

          <Budget usd={d.spend.usd} since={d.spend.since} limit={budget?.limit_usd} onExceed={budget?.on_exceed} owner={me.data?.role === 'owner'} />

          {d.pending_approvals.length > 0 && <Inbox approvals={d.pending_approvals} onDecided={refresh} onOpenRun={setSelectedRun} />}

          <Triggers agentID={id} triggers={d.triggers} onChange={refresh} />

          <Memories agentID={id} runs={d.runs} />

          <section className="space-y-3">
            <SectionHead aside={`${d.runs.length} shown`}>runs</SectionHead>
            <ListGroup empty="No runs yet. Press Run now or add a trigger.">
              {d.runs.map((r) => (
                <li key={r.id}>
                  <button onClick={() => setSelectedRun(r.id)} className={clsx('w-full text-left px-3 py-2', run?.id === r.id ? 'bg-bg-3' : 'hover:bg-bg-2')}>
                    <div className="flex items-center justify-between gap-3">
                      <span className="font-mono text-xs truncate">{r.id.slice(0, 8)}</span>
                      <RunState s={r.status} />
                    </div>
                    <div className="meta truncate">
                      {ago(r.created_at)} · {r.step_count} step{r.step_count === 1 ? '' : 's'} · ${r.cost_usd.toFixed(3)}
                      {r.error ? ` · ${r.error}` : ''}
                    </div>
                  </button>
                </li>
              ))}
            </ListGroup>
          </section>
        </div>
      </div>
      <div className="flex-1 min-w-0 min-h-0 flex flex-col">
        {run ? <RunView key={run.id} run={run} onChange={refresh} /> : <p className="meta p-6">Pick a run to see its steps and transcript.</p>}
      </div>
    </div>
  )
}

function RunState({ s }: { s: AgentRun['status'] }) {
  const label = s === 'paused_approval' ? 'needs approval' : s === 'paused_steer' ? 'needs input' : s
  return <span className={clsx('section-head border-0 shrink-0', OPEN.has(s) ? 'text-fg' : 'text-fg-3')}>{label}</span>
}

function Budget({ usd, since, limit, onExceed, owner }: { usd: number; since: string; limit?: number; onExceed?: string; owner?: boolean }) {
  const month = new Date(since).toLocaleDateString(undefined, { month: 'long' })
  if (!limit) {
    return (
      <p className="meta">
        ${usd.toFixed(2)} spent in {month}. No agent budget is set{owner ? ' (Admin → budgets, scope agent)' : '; the owner can set one'}.
      </p>
    )
  }
  const pct = Math.min(100, Math.round((usd / limit) * 100))
  return (
    <div className="space-y-1.5">
      <p className="meta">
        ${usd.toFixed(2)} of ${limit.toFixed(2)} in {month} ({pct}%). At the limit runs {onExceed === 'block' ? 'stop' : 'fall back to local models'}.
      </p>
      <div className="h-px w-full bg-line relative" aria-hidden>
        <div className="absolute inset-y-0 left-0 bg-fg-3" style={{ width: `${pct}%`, height: '1px' }} />
      </div>
    </div>
  )
}

/** Pending approvals across the agent's runs, decided here. */
function Inbox({ approvals, onDecided, onOpenRun }: { approvals: Approval[]; onDecided: () => void; onOpenRun: (id: string) => void }) {
  const decide = useMutation({
    mutationFn: ({ id, approved }: { id: string; approved: boolean }) => api.post(`/api/approvals/${id}`, { approved }),
    onSuccess: onDecided,
  })
  return (
    <section className="space-y-3">
      <SectionHead aside={`${approvals.length} waiting`}>approvals</SectionHead>
      <ul className="space-y-2">
        {approvals.map((ap) => (
          <li key={ap.id} className="rounded-lg border border-accent/50 bg-bg-2 p-3 text-sm space-y-2">
            <div>
              The agent wants to run <span className="font-mono text-xs">{ap.tool_name}</span>{' '}
              <button onClick={() => onOpenRun(ap.run_id)} className="meta">(run {ap.run_id.slice(0, 8)})</button>
            </div>
            <pre className="max-h-32 overflow-auto font-mono text-xs text-fg-2">{JSON.stringify(ap.args, null, 2)}</pre>
            <div className="flex gap-2">
              <button onClick={() => decide.mutate({ id: ap.id, approved: true })} disabled={decide.isPending} className={btn.primarySm}>
                Allow
              </button>
              <button onClick={() => decide.mutate({ id: ap.id, approved: false })} disabled={decide.isPending} className={btn.secondarySm}>
                Decline
              </button>
            </div>
          </li>
        ))}
      </ul>
      {decide.error && <Callout kind="error">{(decide.error as Error).message}</Callout>}
    </section>
  )
}

function Triggers({ agentID, triggers, onChange }: { agentID: string; triggers: AgentTrigger[]; onChange: () => void }) {
  const [kind, setKind] = useState<'cron' | 'webhook'>('cron')
  const [name, setName] = useState('')
  const [expr, setExpr] = useState('0 * * * *')
  const [input, setInput] = useState('')
  const [secretURL, setSecretURL] = useState<string | null>(null)
  const create = useMutation({
    mutationFn: () =>
      api.post<{ trigger: AgentTrigger; url?: string }>(`/api/agents/${agentID}/triggers`, {
        kind,
        name,
        spec: kind === 'cron' ? { expr, input: input || undefined } : {},
      }),
    onSuccess: (r) => {
      setName('')
      setSecretURL(r.url ? `${location.origin}${r.url}` : null)
      onChange()
    },
  })
  const remove = useMutation({ mutationFn: (tid: string) => api.del(`/api/agents/${agentID}/triggers/${tid}`), onSuccess: onChange })
  const toggle = useMutation({
    mutationFn: ({ tid, enabled }: { tid: string; enabled: boolean }) => api.post(`/api/agents/${agentID}/triggers/${tid}/enabled`, { enabled }),
    onSuccess: onChange,
  })
  return (
    <section className="space-y-3">
      <SectionHead>triggers</SectionHead>
      <ListGroup empty="No triggers. The agent runs only when you press Run now.">
        {triggers.map((t) => (
          <li key={t.id} className="px-3 py-2 space-y-0.5">
            <div className="flex items-center justify-between gap-3">
              <span className="truncate">
                {t.name || t.kind} <span className="font-mono text-xs text-fg-3">{t.kind === 'cron' ? t.spec.expr : t.kind}</span>
              </span>
              <span className="flex items-center gap-3 shrink-0">
                <button onClick={() => toggle.mutate({ tid: t.id, enabled: !t.enabled })} className="text-xs text-fg-2 hover:text-fg">
                  {t.enabled ? 'disable' : 'enable'}
                </button>
                <button onClick={() => remove.mutate(t.id)} className={btn.danger}>
                  remove
                </button>
              </span>
            </div>
            <div className="meta truncate">
              {t.kind === 'cron' && t.next_run_at && t.enabled ? `next ${until(t.next_run_at)}` : t.enabled ? 'enabled' : 'disabled'}
              {t.last_run_at ? ` · fired ${ago(t.last_run_at)}` : ''}
              {t.last_error ? ` · ${t.last_error}` : ''}
              {t.url ? ` · POST ${t.url}` : ''}
            </div>
          </li>
        ))}
      </ListGroup>
      {secretURL && (
        <Callout kind="note">
          Webhook created. Its URL carries the secret and is shown once:
          <code className="block font-mono text-xs mt-1 break-all select-all">{secretURL}</code>
        </Callout>
      )}
      <form
        className="flex flex-wrap items-center gap-2 text-sm"
        onSubmit={(e) => {
          e.preventDefault()
          create.mutate()
        }}
      >
        <select value={kind} onChange={(e) => setKind(e.target.value as 'cron' | 'webhook')} className={`${inputSm} w-auto`} aria-label="Kind">
          <option value="cron">cron</option>
          <option value="webhook">webhook</option>
        </select>
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="name" className={`${inputSm} w-32`} />
        {kind === 'cron' && (
          <>
            <input value={expr} onChange={(e) => setExpr(e.target.value)} placeholder="*/10 * * * *" className={`${inputSm} w-36 font-mono text-xs`} aria-label="Cron expression" />
            <input value={input} onChange={(e) => setInput(e.target.value)} placeholder="message for each run (optional)" className={`${inputSm} flex-1 min-w-40`} />
          </>
        )}
        <button type="submit" disabled={create.isPending || (kind === 'cron' && !expr.trim())} className={btn.secondarySm}>
          Add
        </button>
      </form>
      {create.error && <Callout kind="error">{(create.error as Error).message}</Callout>}
      <p className="meta">Cron is five fields in UTC (or @hourly, @daily). A firing is skipped while a run is still open.</p>
    </section>
  )
}

/** The memory browser: what reflection kept from earlier runs. */
function Memories({ agentID, runs }: { agentID: string; runs: AgentRun[] }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const openRuns = runs.filter((r) => OPEN.has(r.status)).length
  const mems = useQuery({
    queryKey: ['memories', agentID],
    queryFn: () => api.get<{ memories: AgentMemory[]; total: number }>(`/api/agents/${agentID}/memories?limit=100`),
    // Reflection runs shortly after a run ends; keep the list fresh while
    // runs are open and for a while after.
    refetchInterval: openRuns > 0 ? 5000 : 30000,
  })
  const refresh = () => qc.invalidateQueries({ queryKey: ['memories', agentID] })
  const forget = useMutation({ mutationFn: (mid: string) => api.del(`/api/agents/${agentID}/memories/${mid}`), onSuccess: refresh })
  const clear = useMutation({ mutationFn: () => api.del(`/api/agents/${agentID}/memories`), onSuccess: refresh })
  const list = mems.data?.memories ?? []
  const total = mems.data?.total ?? 0
  return (
    <section className="space-y-3">
      <SectionHead aside={total ? `${total} kept` : undefined}>memory</SectionHead>
      {mems.error && <Callout kind="error">{(mems.error as Error).message}</Callout>}
      {mems.isSuccess && list.length === 0 && <p className="meta">Nothing remembered yet. After each run a cheap model writes down the facts, preferences and a short episode; the next run reads the nearest ones.</p>}
      {list.length > 0 && (
        <>
          <ul className="divide-y divide-line rounded-lg border border-line text-sm">
            {(open ? list : list.slice(0, 6)).map((m) => (
              <li key={m.id} className="px-3 py-2 space-y-0.5">
                <div className="flex items-start justify-between gap-3">
                  <p className="reading-tight text-[15px]">{m.content}</p>
                  <button onClick={() => forget.mutate(m.id)} className={clsx(btn.danger, 'shrink-0')}>
                    forget
                  </button>
                </div>
                <div className="meta truncate">
                  {m.kind} · importance {m.importance.toFixed(2)}
                  {m.embedded ? '' : ' · no embedding'}
                  {m.last_used_at ? ` · recalled ${ago(m.last_used_at)}` : ''}
                  {` · ${ago(m.created_at)}`}
                </div>
              </li>
            ))}
          </ul>
          <div className="flex items-center gap-3">
            {list.length > 6 && (
              <button onClick={() => setOpen((v) => !v)} className="text-xs text-fg-2 hover:text-fg">
                {open ? 'show fewer' : `show all ${list.length}`}
              </button>
            )}
            <button
              onClick={() => {
                if (confirm('Forget everything this agent remembers?')) clear.mutate()
              }}
              className={clsx(btn.danger, 'ml-auto')}
            >
              forget all
            </button>
          </div>
        </>
      )}
    </section>
  )
}

type RunDetail = { run: AgentRun; steps: AgentStep[]; approvals: Approval[] }
type ConvResponse = { conversation: Conversation; messages: UIMessage[]; run_status: string }

/** One run: steps, transcript, steer box, cancel. */
function RunView({ run, onChange }: { run: AgentRun; onChange: () => void }) {
  const open = OPEN.has(run.status)
  const detail = useQuery({
    queryKey: ['run', run.id],
    queryFn: () => api.get<RunDetail>(`/api/runs/${run.id}`),
    refetchInterval: open ? 3000 : false,
  })
  const conv = useQuery({
    queryKey: ['conv', run.conversation_id, run.status],
    queryFn: () => api.get<ConvResponse>(`/api/conversations/${run.conversation_id}`),
    refetchInterval: open ? 3000 : false,
  })
  const [text, setText] = useState('')
  const [showSteps, setShowSteps] = useState(false)
  const steer = useMutation({
    mutationFn: () => api.post<AgentRun>(`/api/runs/${run.id}/steer`, { text }),
    onSuccess: () => {
      setText('')
      onChange()
    },
  })
  const cancel = useMutation({ mutationFn: () => api.post(`/api/runs/${run.id}/cancel`, {}), onSuccess: onChange })
  const decide = useMutation({
    mutationFn: ({ id, approved }: { id: string; approved: boolean }) => api.post(`/api/approvals/${id}`, { approved }),
    onSuccess: () => {
      onChange()
      conv.refetch()
    },
  })
  const steps = detail.data?.steps ?? []
  return (
    <>
      <div className="h-12 shrink-0 border-b border-line flex items-center justify-between px-4 gap-3">
        <span className="meta truncate">
          run <span className="font-mono text-xs text-fg-2">{run.id.slice(0, 8)}</span> · {run.status.replace('_', ' ')} · {run.step_count} of {run.max_steps} steps · ${run.cost_usd.toFixed(3)}
          {run.started_at ? ` · started ${ago(run.started_at)}` : ''}
        </span>
        <div className="flex items-center gap-3 shrink-0">
          <button onClick={() => setShowSteps((v) => !v)} className="text-xs text-fg-2 hover:text-fg">
            {showSteps ? 'transcript' : `steps (${steps.length})`}
          </button>
          <Link to={`/c/${run.conversation_id}`} className="text-xs text-fg-2 hover:text-fg">
            open as chat
          </Link>
          {open && (
            <button onClick={() => cancel.mutate()} disabled={cancel.isPending} className={btn.danger}>
              cancel
            </button>
          )}
        </div>
      </div>
      {run.error && (
        <div className="px-4 pt-3">
          <Callout kind="error">{run.error}</Callout>
        </div>
      )}
      {showSteps ? (
        <Steps steps={steps} />
      ) : conv.data ? (
        <MessageList messages={conv.data.messages} status={open ? 'streaming' : 'ready'} onApproval={(id, approved) => decide.mutate({ id, approved })} />
      ) : (
        <p className="meta p-6">Loading…</p>
      )}
      <div className="shrink-0 border-t border-line p-3">
        <form
          className="flex gap-2 items-end"
          onSubmit={(e) => {
            e.preventDefault()
            if (text.trim()) steer.mutate()
          }}
        >
          <textarea
            value={text}
            onChange={(e) => setText(e.target.value)}
            rows={2}
            disabled={open}
            placeholder={open ? 'Wait for the run to finish, or cancel it, before steering.' : 'Steer: your message continues this conversation and starts the next run.'}
            className="flex-1 resize-none rounded-2xl border border-line bg-bg-2 px-4 py-2.5 outline-none focus:border-accent reading disabled:opacity-60"
          />
          <button type="submit" disabled={open || steer.isPending || !text.trim()} className={btn.primarySm}>
            Send
          </button>
        </form>
        {steer.error && <p className="text-xs text-danger mt-1">{(steer.error as Error).message}</p>}
      </div>
    </>
  )
}

function Steps({ steps }: { steps: AgentStep[] }) {
  return (
    <div className="flex-1 min-h-0 overflow-y-auto p-4">
      <ol className="space-y-2 text-sm">
        {steps.map((s) => (
          <li key={s.id} className="rounded-md border border-line px-3 py-2">
            <div className="flex items-center justify-between gap-3">
              <span>
                <span className="font-mono text-xs text-fg-3">{s.seq}</span> {s.kind}
                {s.kind === 'tool' && s.input?.name ? <span className="font-mono text-xs"> {String(s.input.name)}</span> : ''}
                {s.kind === 'llm' && s.output?.endpoint ? <span className="meta"> · {String(s.output.endpoint)}</span> : ''}
              </span>
              <span className="meta shrink-0">
                {s.ended_at ? `${Math.max(0, Math.round((new Date(s.ended_at).getTime() - new Date(s.started_at).getTime()) / 1000))} s` : 'running'}
                {s.usage?.output_tokens ? ` · ${s.usage.output_tokens} out` : ''}
              </span>
            </div>
            {s.error && <div className="text-danger text-xs mt-1">{s.error}</div>}
            {s.kind === 'tool' && s.output?.preview ? <pre className="mt-1 max-h-24 overflow-auto font-mono text-xs text-fg-2 whitespace-pre-wrap">{String(s.output.preview)}</pre> : null}
          </li>
        ))}
        {steps.length === 0 && <li className="meta">No steps yet.</li>}
      </ol>
    </div>
  )
}
