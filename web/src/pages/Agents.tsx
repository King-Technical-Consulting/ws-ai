import { useState } from 'react'
import { Link } from 'react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Agent, type AgentInput, type ModelsResponse, type Project } from '../api'
import { SectionHead, PageTitle, Callout, ListGroup, btn, inputSm } from '../components/ui'

/**
 * Agents (PLAN M7): the list of long-lived agents and the form that makes
 * one. An agent is a goal, a system prompt, a model policy, a tool
 * allowlist with per-tool approval policies, and a step cap; triggers and
 * runs live on its monitor page.
 */
export default function Agents() {
  const qc = useQueryClient()
  const agents = useQuery({ queryKey: ['agents'], queryFn: () => api.get<Agent[]>('/api/agents') })
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models'), staleTime: 60_000 })
  const [showForm, setShowForm] = useState(false)

  const create = useMutation({
    mutationFn: (input: AgentInput) => api.post<Agent>('/api/agents', input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['agents'] })
      setShowForm(false)
    },
  })

  return (
    <div className="flex-1 min-h-0 overflow-y-auto">
      <div className="mx-auto max-w-3xl px-6 py-6 space-y-6">
        <div className="flex items-baseline justify-between gap-4">
          <PageTitle>Agents</PageTitle>
          {!showForm && (
            <button onClick={() => setShowForm(true)} className={btn.primarySm}>
              New agent
            </button>
          )}
        </div>
        <p className="reading text-fg-2">
          An agent works a standing goal on its own: on a schedule, when a webhook fires, or when you press Run. Each run is a conversation in the agent's project, driven by the worker with the tools and approvals you set here.
        </p>

        {showForm && (
          <AgentForm
            projects={projects.data ?? []}
            models={models.data}
            pending={create.isPending}
            error={create.error as Error | null}
            onCancel={() => setShowForm(false)}
            onSubmit={(input) => create.mutate(input)}
          />
        )}

        <section className="space-y-3">
          <SectionHead>agents</SectionHead>
          {agents.error && <Callout kind="error">{(agents.error as Error).message}</Callout>}
          <ListGroup empty="No agents yet.">
            {agents.data?.map((a) => (
              <li key={a.id} className="px-3 py-2">
                <Link to={`/agents/${a.id}`} className="block">
                  <div className="flex items-center justify-between gap-3">
                    <span className="truncate">{a.name}</span>
                    <span className="section-head border-0 shrink-0">{a.enabled ? 'enabled' : 'paused'}</span>
                  </div>
                  <div className="meta truncate">
                    {a.goal || 'no goal set'}
                    {a.last_run_at ? ` · last run ${ago(a.last_run_at)}` : ' · never run'}
                  </div>
                </Link>
              </li>
            ))}
          </ListGroup>
        </section>
      </div>
    </div>
  )
}

const TASK_CLASSES = ['chat', 'code', 'summarize', 'vision']

/** The create and edit form, shared with the monitor page. */
export function AgentForm({
  initial,
  projects,
  models,
  pending,
  error,
  onCancel,
  onSubmit,
}: {
  initial?: Agent
  projects: Project[]
  models?: ModelsResponse
  pending: boolean
  error: Error | null
  onCancel: () => void
  onSubmit: (input: AgentInput) => void
}) {
  const [name, setName] = useState(initial?.name ?? '')
  const [goal, setGoal] = useState(initial?.goal ?? '')
  const [systemPrompt, setSystemPrompt] = useState(initial?.system_prompt ?? '')
  const [projectID, setProjectID] = useState(initial?.project_id ?? projects[0]?.id ?? '')
  const [selector, setSelector] = useState(initial?.model_policy?.selector ?? 'auto')
  const [taskClass, setTaskClass] = useState(initial?.model_policy?.task_class ?? 'chat')
  const [reasoning, setReasoning] = useState(initial?.model_policy?.reasoning ?? false)
  const [allow, setAllow] = useState((initial?.tool_allowlist ?? []).join(', '))
  const [policies, setPolicies] = useState(JSON.stringify(initial?.tool_policies ?? {}, null, 0))
  const [maxSteps, setMaxSteps] = useState(initial?.max_steps ?? 50)
  const [localError, setLocalError] = useState('')

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    let tp: Record<string, 'auto' | 'ask' | 'deny'> = {}
    try {
      tp = policies.trim() ? JSON.parse(policies) : {}
    } catch {
      setLocalError('Tool policies must be JSON, for example {"bash":"ask"}.')
      return
    }
    setLocalError('')
    onSubmit({
      name,
      goal,
      system_prompt: systemPrompt,
      project_id: projectID,
      model: { selector, task_class: taskClass, reasoning },
      tool_allowlist: allow.split(',').map((s) => s.trim()).filter(Boolean),
      tool_policies: tp,
      max_steps: maxSteps,
      enabled: initial?.enabled,
    })
  }

  return (
    <form onSubmit={submit} className="space-y-3 rounded-lg border border-line bg-bg-2 p-4 text-sm">
      <SectionHead>{initial ? 'edit agent' : 'new agent'}</SectionHead>
      <label className="block space-y-1">
        <span className="text-xs text-fg-2">Name</span>
        <input value={name} onChange={(e) => setName(e.target.value)} className={inputSm} required autoFocus />
      </label>
      <label className="block space-y-1">
        <span className="text-xs text-fg-2">Goal</span>
        <textarea value={goal} onChange={(e) => setGoal(e.target.value)} rows={2} className={`${inputSm} reading-tight text-[15px]`} placeholder="What the agent works on every run." />
      </label>
      <label className="block space-y-1">
        <span className="text-xs text-fg-2">System prompt (optional)</span>
        <textarea value={systemPrompt} onChange={(e) => setSystemPrompt(e.target.value)} rows={3} className={`${inputSm} reading-tight text-[15px]`} placeholder="Replaces the default persona; the goal and the unattended-run rules are appended." />
      </label>
      <div className="grid grid-cols-2 gap-3">
        <label className="block space-y-1">
          <span className="text-xs text-fg-2">Project</span>
          <select value={projectID} onChange={(e) => setProjectID(e.target.value)} className={inputSm} required>
            <option value="">pick a project</option>
            {projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name} · {p.kind}
              </option>
            ))}
          </select>
        </label>
        <label className="block space-y-1">
          <span className="text-xs text-fg-2">Model</span>
          <select value={selector} onChange={(e) => setSelector(e.target.value)} className={inputSm}>
            {Object.keys(models?.aliases ?? { auto: null }).map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
            {models?.models.map((m) => (
              <option key={m.id} value={m.id}>
                {m.display_name}
              </option>
            ))}
          </select>
        </label>
        <label className="block space-y-1">
          <span className="text-xs text-fg-2">Task class</span>
          <select value={taskClass} onChange={(e) => setTaskClass(e.target.value)} className={inputSm}>
            {TASK_CLASSES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </label>
        <label className="block space-y-1">
          <span className="text-xs text-fg-2">Max steps per run</span>
          <input type="number" min={1} max={500} value={maxSteps} onChange={(e) => setMaxSteps(Number(e.target.value))} className={inputSm} />
        </label>
      </div>
      <label className="flex items-center gap-2 text-xs text-fg-2">
        <input type="checkbox" className="accent-accent" checked={reasoning} onChange={(e) => setReasoning(e.target.checked)} /> extended reasoning
      </label>
      <label className="block space-y-1">
        <span className="text-xs text-fg-2">Tool allowlist (comma separated; empty means every tool the worker has)</span>
        <input value={allow} onChange={(e) => setAllow(e.target.value)} className={`${inputSm} font-mono text-xs`} placeholder="web_fetch, create_artifact, bash" />
      </label>
      <label className="block space-y-1">
        <span className="text-xs text-fg-2">Tool policies (JSON: auto, ask or deny per tool; ask pauses the run for your approval)</span>
        <input value={policies} onChange={(e) => setPolicies(e.target.value)} className={`${inputSm} font-mono text-xs`} placeholder='{"bash":"ask","git_push":"ask"}' />
      </label>
      {(localError || error) && <Callout kind="error">{localError || error?.message}</Callout>}
      <div className="flex gap-2">
        <button type="submit" disabled={pending || !name.trim() || !projectID} className={btn.primarySm}>
          {initial ? 'Save' : 'Create'}
        </button>
        <button type="button" onClick={onCancel} className={btn.secondarySm}>
          Cancel
        </button>
      </div>
    </form>
  )
}

export function ago(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  if (!Number.isFinite(ms) || ms < 0) return 'soon'
  const m = Math.floor(ms / 60_000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}

export function until(iso: string): string {
  const ms = new Date(iso).getTime() - Date.now()
  if (!Number.isFinite(ms) || ms < 0) return 'now'
  const m = Math.ceil(ms / 60_000)
  if (m < 60) return `in ${m}m`
  const h = Math.floor(m / 60)
  if (h < 48) return `in ${h}h`
  return `in ${Math.floor(h / 24)}d`
}
