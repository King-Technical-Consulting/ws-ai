// Thin fetch wrapper for the Go API. Cookies carry the session.

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function req<T>(method: string, path: string, body?: unknown, raw?: BodyInit): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
    body: raw ?? (body !== undefined ? JSON.stringify(body) : undefined),
    credentials: 'same-origin',
  })
  if (!res.ok) {
    let msg = res.statusText
    try {
      const j = await res.json()
      if (j?.error) msg = j.error
    } catch {}
    throw new ApiError(res.status, msg)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export const api = {
  get: <T>(p: string) => req<T>('GET', p),
  post: <T>(p: string, b?: unknown) => req<T>('POST', p, b),
  postRaw: <T>(p: string, raw: BodyInit) => req<T>('POST', p, undefined, raw),
  patch: <T>(p: string, b?: unknown) => req<T>('PATCH', p, b),
  put: <T>(p: string, b?: unknown) => req<T>('PUT', p, b),
  del: <T>(p: string) => req<T>('DELETE', p),
}

// ---- types mirrored from the Go side ----

export type User = { id: string; email: string; display_name: string; role: 'owner' | 'member'; via_api_key?: boolean; training_consent?: boolean }

// ---- training flywheel (PLAN M10) ----

export type DatasetFilters = {
  modes?: string[]
  models?: string[]
  min_rating: number
  since?: string
  holdout_pct: number
  max_examples?: number
}

export type Dataset = {
  id: string
  owner_id: string
  name: string
  task_class: string
  filters: DatasetFilters
  status: 'queued' | 'building' | 'ready' | 'failed'
  blob_key: string | null
  eval_blob_key: string | null
  bytes: number
  examples: number
  eval_examples: number
  error: string | null
  created_at: string
  built_at: string | null
}

export type FinetuneConfig = { epochs?: number; learning_rate?: number; rank?: number; alpha?: number; max_seq_len?: number; image?: string; target?: 'local' | 'rental' }
export type FinetuneTarget = { id: 'local' | 'rental'; label: string }

export type FinetuneJob = {
  id: string
  owner_id: string
  dataset_id: { UUID: string; Valid: boolean }
  base_model: string
  base_endpoint_id: string | null
  adapter_name: string
  config: FinetuneConfig
  status: 'queued' | 'running' | 'done' | 'failed' | 'cancelled'
  progress: number
  log: string
  adapter_id: { UUID: string; Valid: boolean }
  error: string | null
  created_at: string
  started_at: string | null
  ended_at: string | null
}

export type Adapter = {
  id: string
  name: string
  base_model: string
  base_endpoint_id: string | null
  finetune_job_id: { UUID: string; Valid: boolean }
  blob_key: string
  bytes: number
  eval_score: number | null
  baseline_score: number | null
  eval: { examples: number; adapter_score: number; baseline_score: number; judge: string; adapter_wins: number; baseline_wins: number; ties: number; errors?: string[]; at: string } | null
  promoted: boolean
  endpoint_id: string | null
  created_at: string
  evaluated_at: string | null
}

export type Project = {
  id: string
  owner_id: string
  name: string
  kind: 'chat' | 'code' | 'design' | 'images'
  created_at: string
  updated_at: string
}

// The design system a design conversation's mockups follow (PLAN M6);
// stored in the conversation's settings and on each design artifact
// version as design_context.
export type DesignContext = {
  library?: 'tailwind' | 'shadcn' | 'plain'
  colors?: Record<string, string>
  type?: Record<string, string>
  spacing?: string
  radius?: string
  components?: string[]
  notes?: string
}

export type ConversationSettings = {
  tool_policies?: Record<string, 'auto' | 'ask' | 'deny'>
  design_context?: DesignContext
}

export type Conversation = {
  id: string
  project_id: string
  user_id: string
  title: string
  mode: string
  model_selector: string
  settings?: ConversationSettings | null
  created_at: string
  updated_at: string
}

export type ModelInfo = {
  id: string
  display_name: string
  provider: string
  local: boolean
  capabilities: { context_window: number; tools: boolean; vision: boolean; reasoning: boolean }
  pricing: { input_per_m: number; output_per_m: number }
  health: string
}

export type ModelsResponse = { models: ModelInfo[]; aliases: Record<string, string[] | null>; media?: MediaModel[] }

// A Claude Code job handle (cc_jobs): where a tmux window lives, never
// what is in it.
export type CCJob = {
  id: string
  target: string
  session: string
  window: string
  cwd: string
  lane: string
  model: string | null
  source: 'wsj' | 'router' | 'tmux'
  status: 'alive' | 'dead' | 'killed' | 'gone' | 'unknown'
  started_at: string
  seen_at: string | null
  ended_at: string | null
}

export type CCJobsResponse = {
  jobs: CCJob[]
  refresh: { at: string; targets: { name: string; ok: boolean; error?: string; windows: number }[]; error?: string }
  // The week's subscription launch pool (cc_launch_counter); cap 0 means count only.
  budget?: { week: string; used: number; cap: number; soft_pct: number; level: 'ok' | 'soft' | 'full' }
}

// ---- media (images and video, PLAN M6) ----

// A media endpoint from GET /api/models `media`: what it makes and what it costs.
export type MediaModel = {
  id: string
  display_name: string
  provider: string
  local: boolean
  engine: string
  image: boolean
  image_edit: boolean
  video: boolean
  image_to_video: boolean
  upscale: boolean
  sizes: string[]
  max_images: number
  max_seconds: number
  seconds: number[]
  per_image: number
  per_second: number
  health: string
}

export type Attachment = {
  id: string
  url: string
  mime: string
  bytes: number
  filename: string
  width: number | null
  height: number | null
}

// A media_jobs row with its outputs expanded.
export type MediaJob = {
  id: string
  user_id: string
  project_id: string
  conversation_id: { UUID: string; Valid: boolean } | null
  kind: 'image' | 'video' | 'edit' | 'upscale'
  selector: string
  endpoint_id: string | null
  inputs: { prompt: string; size?: string; n?: number; quality?: string; seconds?: number; source_attachment_id?: string; mask_attachment_id?: string; scale?: number; estimate_usd?: number }
  provider_job_id: string | null
  status: 'queued' | 'running' | 'done' | 'failed' | 'cancelled'
  progress: number
  output_attachment_ids: string[]
  cost_usd: number
  error: string | null
  created_at: string
  started_at: string | null
  ended_at: string | null
  outputs: Attachment[]
}

// ---- long-lived agents (PLAN M7) ----

export type NullUUID = { UUID: string; Valid: boolean }

export type Agent = {
  id: string
  owner_id: string
  project_id: string | null
  name: string
  goal: string
  system_prompt: string
  model_policy: { selector?: string; task_class?: string; reasoning?: boolean }
  tool_allowlist: string[]
  tool_policies: Record<string, 'auto' | 'ask' | 'deny'>
  max_steps: number
  enabled: boolean
  last_run_at: string | null
  created_at: string
  updated_at: string
}

export type AgentInput = {
  name: string
  goal: string
  system_prompt: string
  project_id: string
  model: { selector?: string; task_class?: string; reasoning?: boolean }
  tool_allowlist: string[]
  tool_policies: Record<string, 'auto' | 'ask' | 'deny'>
  max_steps: number
  enabled?: boolean
}

export type AgentPreset = {
  name: string
  title: string
  description: string
  goal: string
  prompt: string
  model: { selector?: string; task_class?: string; reasoning?: boolean }
  tools: string[]
  max_steps: number
  note: string
}

export type SkillImport = { preset: AgentPreset; requested_tools: string[]; warnings: string[] }

export type AgentTrigger = {
  id: string
  agent_id: string
  kind: 'cron' | 'webhook' | 'repo_push' | 'manual'
  name: string
  spec: { expr?: string; input?: string; repo?: string; branches?: string[]; events?: string[] }
  enabled: boolean
  created_at: string
  next_run_at: string | null
  last_run_at: string | null
  last_error: string | null
  url?: string
}

export type AgentRun = {
  id: string
  agent_id: NullUUID
  conversation_id: string
  trigger_id: NullUUID
  status: 'queued' | 'running' | 'paused_approval' | 'paused_steer' | 'paused_manual' | 'done' | 'failed' | 'cancelled'
  max_steps: number
  step_count: number
  cost_usd: number
  error: string | null
  started_at: string | null
  ended_at: string | null
  created_at: string
}

export type Approval = {
  id: string
  run_id: string
  tool_call_id: string
  tool_name: string
  args: unknown
  status: 'pending' | 'approved' | 'denied' | 'expired'
  note: string | null
  created_at: string
}

export type AgentStep = {
  id: string
  seq: number
  kind: 'llm' | 'tool' | 'approval' | 'compaction'
  input: Record<string, unknown> | null
  output: Record<string, unknown> | null
  usage: { input_tokens?: number; output_tokens?: number } | null
  error: string | null
  started_at: string
  ended_at: string | null
}

export type AgentDetail = {
  agent: Agent
  triggers: AgentTrigger[]
  runs: AgentRun[]
  pending_approvals: Approval[]
  spend: { since: string; usd: number; budgets: { id: string; period: string; limit_usd: number; on_exceed: string }[] }
}

export type AgentMemory = {
  id: string
  kind: 'fact' | 'episode' | 'preference'
  content: string
  importance: number
  embedded: boolean
  source_run_id: NullUUID
  last_used_at: string | null
  created_at: string
}
