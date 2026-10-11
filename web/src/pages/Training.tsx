import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { api, type Adapter, type Dataset, type FinetuneJob, type FinetuneTarget, type FinetuneTargetID, type ModelsResponse, type TrainingExample, type TrainingRoute } from '../api'
import { SectionHead, PageTitle, Callout, ListGroup, Row, btn, inputSm, useConfirm } from '../components/ui'
import { ago } from './Agents'
import { DatasetUpload, UploadedDetails } from '../components/DatasetUpload'
import { BaseModelPicker } from '../components/BaseModelPicker'
import { Recipes } from '../components/Recipes'
import { DEFAULT_SMOKE_EVAL_EXAMPLES, isSmokeEval, pointsPerExample, smokeTitle } from '../lib/evalSize'

const MODES = ['chat', 'code', 'design', 'agent']
// Task classes a route or a promotion can name (config/policies/default.yaml).
const TASK_CLASSES = ['chat', 'code', 'summarize', 'title', 'reflect', 'classify', 'vision']

/**
 * Training (PLAN M10, owner only): datasets exported from opted-in
 * conversations, fine-tune jobs that make adapters, and the adapters
 * with their eval scores and promotion.
 */
export default function Training() {
  const qc = useQueryClient()
  const datasets = useQuery({
    queryKey: ['training', 'datasets'],
    queryFn: () => api.get<{ datasets: Dataset[] }>('/api/training/datasets'),
    refetchInterval: (q) => (q.state.data?.datasets.some((d) => d.status === 'queued' || d.status === 'building') ? 2000 : false),
  })
  const jobs = useQuery({
    queryKey: ['training', 'jobs'],
    queryFn: () => api.get<{ jobs: FinetuneJob[]; runner: boolean; targets: FinetuneTarget[] }>('/api/training/jobs'),
    refetchInterval: (q) => (q.state.data?.jobs.some((j) => j.status === 'queued' || j.status === 'running') ? 3000 : false),
  })
  const adapters = useQuery({ queryKey: ['training', 'adapters'], queryFn: () => api.get<{ adapters: Adapter[]; smoke_eval_examples?: number }>('/api/training/adapters'), refetchInterval: (q) => (q.state.data?.adapters.some((a) => a.eval_job?.state === 'queued' || a.eval_job?.state === 'running') ? 3000 : 10000) })
  const routes = useQuery({ queryKey: ['training', 'routes'], queryFn: () => api.get<{ routes: TrainingRoute[]; policy: string }>('/api/training/routes') })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models'), staleTime: 60_000 })
  const refresh = () => qc.invalidateQueries({ queryKey: ['training'] })
  // Under this many held-out examples an eval is a smoke result; the server owns the number.
  const smoke = adapters.data?.smoke_eval_examples ?? DEFAULT_SMOKE_EVAL_EXAMPLES

  return (
    <div className="flex-1 min-h-0 overflow-y-auto">
      <div className="mx-auto max-w-4xl px-6 py-6 space-y-10">
        <PageTitle>Training</PageTitle>
        <p className="text-sm text-fg-2">
          Rated answers from people who opted in become datasets; a dataset and a base model become an adapter; an adapter that beats its base on the held-out set gets promoted to an endpoint. Operational notes are in <code className="font-mono text-xs">infra/training/README.md</code>.
        </p>
        <Datasets datasets={datasets.data?.datasets ?? []} smoke={smoke} onChange={refresh} />
        <DatasetUpload onChange={refresh} />
        <Recipes datasets={datasets.data?.datasets ?? []} smoke={smoke} />
        <Jobs jobs={jobs.data?.jobs ?? []} runner={!!jobs.data?.runner} targets={jobs.data?.targets ?? []} datasets={datasets.data?.datasets ?? []} models={models.data} onChange={refresh} />
        <Adapters adapters={adapters.data?.adapters ?? []} smoke={smoke} onChange={refresh} />
        <Routes routes={routes.data?.routes ?? []} policy={routes.data?.policy ?? 'training-adapters'} models={models.data} onChange={refresh} />
      </div>
    </div>
  )
}

function StatusWord({ s }: { s: string }) {
  const open = s === 'queued' || s === 'building' || s === 'running'
  return <span className={clsx('section-head border-0 shrink-0', open ? 'text-fg' : s === 'failed' ? 'text-danger' : 'text-fg-3')}>{s}</span>
}

function Datasets({ datasets, smoke, onChange }: { datasets: Dataset[]; smoke: number; onChange: () => void }) {
  const [name, setName] = useState('')
  const [modes, setModes] = useState<string[]>([])
  const [modelsText, setModelsText] = useState('')
  const [minRating, setMinRating] = useState(0)
  const [since, setSince] = useState('')
  const [holdout, setHoldout] = useState(10)
  const create = useMutation({
    mutationFn: () =>
      api.post<Dataset>('/api/training/datasets', {
        name,
        filters: {
          modes,
          models: modelsText.split(',').map((s) => s.trim()).filter(Boolean),
          min_rating: minRating,
          since: since ? new Date(since).toISOString() : undefined,
          holdout_pct: holdout,
        },
      }),
    onSuccess: () => {
      setName('')
      onChange()
    },
  })
  const remove = useMutation({ mutationFn: (id: string) => api.del(`/api/training/datasets/${id}`), onSuccess: onChange })
  const [previewID, setPreviewID] = useState<string | null>(null)
  const [detailsID, setDetailsID] = useState<string | null>(null)
  return (
    <section className="space-y-3">
      <SectionHead aside={datasets.length ? `${datasets.length}` : undefined}>datasets</SectionHead>
      <ListGroup empty="No datasets yet.">
        {datasets.map((d) => (
          <Row
            key={d.id}
            action={
              <span className="flex flex-wrap items-center gap-x-3">
                {d.kind === 'uploaded' && (
                  <button onClick={() => setDetailsID(detailsID === d.id ? null : d.id)} className={btn.link}>
                    {detailsID === d.id ? 'hide details' : 'details'}
                  </button>
                )}
                {d.status === 'ready' && (
                  <>
                    <button onClick={() => setPreviewID(previewID === d.id ? null : d.id)} className={btn.link}>
                      {previewID === d.id ? 'hide' : 'preview'}
                    </button>
                    <a href={`/api/training/datasets/${d.id}/download`} download title="Download the train split as JSONL" className={btn.link}>train.jsonl</a>
                    {d.eval_examples > 0 && <a href={`/api/training/datasets/${d.id}/download?split=eval`} download title="Download the held-out split as JSONL" className={btn.link}>eval.jsonl</a>}
                  </>
                )}
                <button onClick={() => remove.mutate(d.id)} className={btn.danger}>remove</button>
              </span>
            }
          >
            <span className="flex flex-wrap items-baseline gap-x-2">
              <StatusWord s={d.status} />
              {d.name}
            </span>
            <span className="meta block">
              {d.status === 'ready' ? `${d.examples} examples, ${d.eval_examples} held out · ${(d.bytes / 1024).toFixed(0)} KB · ` : ''}
              {d.status === 'ready' && isSmokeEval(d.eval_examples, smoke) ? `an eval on its ${d.eval_examples} held out would be a smoke result (under ${smoke}) · ` : ''}
              {d.kind === 'merged' ? 'merged from a recipe' : d.kind === 'uploaded' ? `uploaded ${d.source_name ?? ''}` : (d.filters.modes?.length ? d.filters.modes.join(', ') + ' · ' : '') + (d.filters.min_rating === 1 ? 'upvoted only' : d.filters.min_rating === -1 ? 'all ratings' : 'upvoted or unrated')} · {ago(d.created_at)}
              {d.error ? ` · ${d.error}` : ''}
            </span>
          </Row>
        ))}
      </ListGroup>
      {previewID && <Preview id={previewID} />}
      {datasets.filter((d) => d.id === detailsID && d.kind === 'uploaded').map((d) => <UploadedDetails key={d.id + d.status + (d.built_at ?? '')} dataset={d} onChange={onChange} />)}
      <form
        className="space-y-2 rounded-lg border border-line bg-bg-2 p-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (name.trim()) create.mutate()
        }}
      >
        <div className="flex flex-wrap gap-2">
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Dataset name" className={clsx(inputSm, 'flex-1 min-w-48')} />
          <select value={minRating} onChange={(e) => setMinRating(Number(e.target.value))} className={clsx(inputSm, 'w-auto')} aria-label="Rating floor">
            <option value={1}>upvoted answers only</option>
            <option value={0}>upvoted or unrated</option>
            <option value={-1}>everything, downvoted too</option>
          </select>
          <label className="flex items-center gap-1 text-xs text-fg-2">
            held out <input type="number" min={0} max={50} value={holdout} onChange={(e) => setHoldout(Number(e.target.value))} className={clsx(inputSm, 'w-16 tnum')} aria-label="Held-out percent" /> %
          </label>
          <input type="date" value={since} onChange={(e) => setSince(e.target.value)} className={clsx(inputSm, 'w-auto')} aria-label="Conversations since" title="Conversations active since this date; empty takes all" />
        </div>
        <div className="flex flex-wrap items-center gap-3 text-xs text-fg-2">
          {MODES.map((m) => (
            <label key={m} className="flex items-center gap-1">
              <input type="checkbox" className="accent-accent" checked={modes.includes(m)} onChange={(e) => setModes(e.target.checked ? [...modes, m] : modes.filter((x) => x !== m))} /> {m}
            </label>
          ))}
          <span className="text-fg-3">(none checked: every mode)</span>
          <input value={modelsText} onChange={(e) => setModelsText(e.target.value)} placeholder="answers from these models only (ids, comma separated; empty: any)" className={clsx(inputSm, 'flex-1 min-w-64 font-mono')} />
          <button type="submit" disabled={create.isPending || !name.trim()} className={btn.primarySm}>Build</button>
        </div>
        {create.error && <Callout kind="error">{create.error.message}</Callout>}
      </form>
    </section>
  )
}

/** The first examples of a dataset, as a trainer would read them. */
function Preview({ id }: { id: string }) {
  const [split, setSplit] = useState<'train' | 'eval'>('train')
  const q = useQuery({ queryKey: ['training', 'preview', id, split], queryFn: () => api.get<{ examples: TrainingExample[] }>(`/api/training/datasets/${id}/preview?split=${split}&n=5`) })
  const exs = q.data?.examples ?? []
  return (
    <div className="space-y-2 rounded-lg border border-line bg-bg-2 p-3">
      <div className="flex items-center gap-3 text-xs text-fg-2">
        <span className="section-head border-0">preview</span>
        <button onClick={() => setSplit('train')} className={clsx('hover:text-fg', split === 'train' && 'text-fg')}>train</button>
        <button onClick={() => setSplit('eval')} className={clsx('hover:text-fg', split === 'eval' && 'text-fg')}>held out</button>
        <span className="text-fg-3">first {exs.length} of the split</span>
      </div>
      {q.error && <Callout kind="error">{q.error.message}</Callout>}
      {exs.map((e, i) => (
        <div key={e.meta.message_id || e.meta.source || i} className="reading space-y-1 border-t border-line pt-2 text-sm">
          {e.messages.map((m, j) => (
            <p key={j}>
              <span className="section-head border-0 mr-2">{m.role}</span>
              {m.content || (m.tool_calls ? m.tool_calls.map((c) => `${c.function.name}(${c.function.arguments})`).join(', ') : '')}
            </p>
          ))}
          <p className="meta">
            {e.meta.source ?? ''} {e.meta.mode ?? ''} {e.meta.model ? `· ${e.meta.model}` : ''} {e.meta.rating ? `· rated ${e.meta.rating > 0 ? 'up' : 'down'}` : ''}
          </p>
        </div>
      ))}
    </div>
  )
}

/**
 * The training-adapters policy: one rule per task class, routed endpoints
 * first (a promoted adapter, or a frontier model whose answers a later
 * dataset collects: distillation), then what the other policies prefer.
 */
function Routes({ routes, policy, models, onChange }: { routes: TrainingRoute[]; policy: string; models?: ModelsResponse; onChange: () => void }) {
  const [taskClass, setTaskClass] = useState('chat')
  const [endpoint, setEndpoint] = useState('')
  const [err, setErr] = useState('')
  const all = models?.models ?? []
  const add = useMutation({ mutationFn: () => api.put('/api/training/routes', { task_class: taskClass, endpoint_id: endpoint }), onSuccess: () => { setErr(''); onChange() }, onError: (e) => setErr(e.message) })
  const remove = useMutation({ mutationFn: (r: TrainingRoute) => api.post('/api/training/routes/remove', { task_class: r.task_class, endpoint_id: r.endpoint }), onSuccess: onChange, onError: (e) => setErr(e.message) })
  return (
    <section className="space-y-3">
      <SectionHead aside={routes.length ? `${routes.length}` : undefined}>routes</SectionHead>
      <p className="text-sm text-fg-2">
        Where a task class goes first, written into the <code className="font-mono text-xs">{policy}</code> policy ahead of the default one: an adapter promoted into a class lands here, and routing a class to a frontier model collects its answers for a distillation dataset (filter that dataset by the model). The rest of the class's usual list stays behind it, so a box that is down still fails over.
      </p>
      <ListGroup empty="No routes: every class follows the default policy.">
        {routes.map((r) => (
          <Row key={r.task_class + r.endpoint} action={<button onClick={() => remove.mutate(r)} className={btn.danger}>remove</button>}>
            <span className="flex flex-wrap items-baseline gap-x-2">
              <span className="section-head border-0 shrink-0">{r.task_class}</span>
              <span className="font-mono text-xs">{r.endpoint}</span>
            </span>
          </Row>
        ))}
      </ListGroup>
      <form
        className="flex flex-wrap items-center gap-2 rounded-lg border border-line bg-bg-2 p-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (endpoint) add.mutate()
        }}
      >
        <select value={taskClass} onChange={(e) => setTaskClass(e.target.value)} className={clsx(inputSm, 'w-auto')} aria-label="Task class">
          {TASK_CLASSES.map((c) => (
            <option key={c} value={c}>{c}</option>
          ))}
        </select>
        <span className="text-xs text-fg-2">goes first to</span>
        <select value={endpoint} onChange={(e) => setEndpoint(e.target.value)} className={clsx(inputSm, 'w-auto max-w-[20rem]')} aria-label="Endpoint">
          <option value="">pick an endpoint</option>
          {all.map((m) => (
            <option key={m.id} value={m.id}>{m.display_name} ({m.id})</option>
          ))}
        </select>
        <button type="submit" disabled={add.isPending || !endpoint} className={btn.primarySm}>Route</button>
      </form>
      {err && <Callout kind="error">{err}</Callout>}
    </section>
  )
}

function Jobs({ jobs, runner, targets, datasets, models, onChange }: { jobs: FinetuneJob[]; runner: boolean; targets: FinetuneTarget[]; datasets: Dataset[]; models?: ModelsResponse; onChange: () => void }) {
  const ready = datasets.filter((d) => d.status === 'ready')
  const local = (models?.models ?? []).filter((m) => m.local)
  const [datasetID, setDatasetID] = useState('')
  const [base, setBase] = useState('')
  const [baseModel, setBaseModel] = useState('')
  const [name, setName] = useState('')
  const [epochs, setEpochs] = useState(2)
  const [rank, setRank] = useState(16)
  const [lr, setLr] = useState('2e-4')
  const [target, setTarget] = useState('')
  const [showLog, setShowLog] = useState<string | null>(null)
  const start = useMutation({
    mutationFn: () =>
      api.post<FinetuneJob>('/api/training/jobs', {
        dataset_id: datasetID || ready[0]?.id,
        base_endpoint_id: base || undefined,
        base_model: baseModel || undefined,
        adapter_name: name,
        config: { epochs, rank, learning_rate: Number(lr) || undefined, target: (target || undefined) as FinetuneTargetID | undefined },
      }),
    onSuccess: () => {
      setName('')
      onChange()
    },
  })
  const cancel = useMutation({ mutationFn: (id: string) => api.post(`/api/training/jobs/${id}/cancel`, {}), onSuccess: onChange })
  const log = jobs.find((j) => j.id === showLog)
  return (
    <section className="space-y-3">
      <SectionHead aside={jobs.length ? `${jobs.length}` : undefined}>fine-tune jobs</SectionHead>
      {!runner && (
        <Callout>
          No fine-tune runner on the worker: set <code className="font-mono text-xs">WS_FINETUNE_IMAGE</code> where Docker and a GPU are, <code className="font-mono text-xs">WS_FINETUNE_URL</code> to a trainer box on the tailnet (the Orin, the Spark or a Mac running the trainer), or <code className="font-mono text-xs">WS_FINETUNE_TEMPLATE</code> to a trainer rental template to train on a rented GPU (see <code className="font-mono text-xs">infra/training</code>). Datasets and ratings work without it.
        </Callout>
      )}
      <ListGroup empty="No jobs yet.">
        {jobs.map((j) => (
          <Row
            key={j.id}
            action={
              <span className="flex flex-wrap items-center gap-x-3">
                <button onClick={() => setShowLog(showLog === j.id ? null : j.id)} className={btn.link}>
                  {showLog === j.id ? 'hide log' : 'log'}
                </button>
                {(j.status === 'queued' || j.status === 'running') && (
                  <button onClick={() => cancel.mutate(j.id)} className={btn.danger}>cancel</button>
                )}
              </span>
            }
          >
            <span className="flex flex-wrap items-baseline gap-x-2">
              <StatusWord s={j.status} />
              {j.adapter_name} <span className="meta">on {j.base_model}</span>
            </span>
            <span className="meta block">
              {j.status === 'running' ? `${Math.round(j.progress * 100)}% · ` : ''}
              {j.config.epochs ?? 2} epochs, rank {j.config.rank ?? 16}{j.config.target === 'rental' ? ' · rented GPU' : j.config.target === 'remote' ? ' · trainer box' : ''} · {ago(j.created_at)}
              {j.error ? ` · ${j.error}` : ''}
            </span>
          </Row>
        ))}
      </ListGroup>
      {log && <pre className="max-h-64 overflow-auto rounded-lg border border-line bg-bg-2 p-3 font-mono text-xs whitespace-pre-wrap">{log.log || '(no output yet)'}</pre>}
      <form
        className="space-y-2 rounded-lg border border-line bg-bg-2 p-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (name.trim()) start.mutate()
        }}
      >
        <div className="flex flex-wrap gap-2">
          <select value={datasetID || ready[0]?.id || ''} onChange={(e) => setDatasetID(e.target.value)} className={clsx(inputSm, 'w-auto max-w-[16rem]')} aria-label="Dataset">
            {ready.length === 0 && <option value="">no ready dataset</option>}
            {ready.map((d) => (
              <option key={d.id} value={d.id}>{d.name} ({d.examples})</option>
            ))}
          </select>
          <select value={base} onChange={(e) => { setBase(e.target.value); const m = local.find((x) => x.id === e.target.value); if (m && !baseModel) setBaseModel('') }} className={clsx(inputSm, 'w-auto max-w-[16rem]')} aria-label="Base endpoint">
            <option value="">base endpoint (optional)</option>
            {local.map((m) => (
              <option key={m.id} value={m.id}>{m.display_name}</option>
            ))}
          </select>
          <BaseModelPicker value={baseModel} onChange={setBaseModel} />
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="adapter name, e.g. chat-v1" className={clsx(inputSm, 'w-48 font-mono')} />
          <label className="flex items-center gap-1 text-xs text-fg-2">epochs <input type="number" min={1} max={20} value={epochs} onChange={(e) => setEpochs(Number(e.target.value))} className={clsx(inputSm, 'w-16 tnum')} /></label>
          <label className="flex items-center gap-1 text-xs text-fg-2">rank <input type="number" min={4} max={256} value={rank} onChange={(e) => setRank(Number(e.target.value))} className={clsx(inputSm, 'w-16 tnum')} /></label>
          <label className="flex items-center gap-1 text-xs text-fg-2">lr <input value={lr} onChange={(e) => setLr(e.target.value)} className={clsx(inputSm, 'w-20 font-mono')} /></label>
          {targets.length > 1 && (
            <select value={target} onChange={(e) => setTarget(e.target.value)} className={clsx(inputSm, 'w-auto')} aria-label="Run on" title="Where the trainer runs; a rented GPU is billed by the hour to the ledger and stopped when the job ends">
              {targets.map((t) => (
                <option key={t.id} value={t.id}>on {t.label}</option>
              ))}
            </select>
          )}
          <button type="submit" disabled={start.isPending || !runner || !name.trim() || ready.length === 0} className={btn.primarySm}>Fine-tune</button>
        </div>
        {start.error && <Callout kind="error">{start.error.message}</Callout>}
      </form>
    </section>
  )
}

/** The sentence the promote confirmations add when the eval rests on few examples. */
function smokeBody(examples: number, smoke: number) {
  return `The eval had only ${examples} held-out examples (fewer than ${smoke}), and one example moves the score by ${pointsPerExample(examples)} points: the verdict is a smoke result, not a measurement.`
}

function Adapters({ adapters, smoke, onChange }: { adapters: Adapter[]; smoke: number; onChange: () => void }) {
  const confirmDialog = useConfirm()
  const [err, setErr] = useState('')
  // The task class a promotion routes to; '' enables the endpoint only.
  const [taskClass, setTaskClass] = useState('chat')
  // The server runs the worker's own checks before it queues anything, so a click that cannot work
  // answers at once with the reason, and so does a second click while one is queued or running.
  const evaluate = useMutation({ mutationFn: (id: string) => api.post(`/api/training/adapters/${id}/evaluate`, {}), onMutate: () => setErr(''), onSuccess: onChange, onError: (e) => setErr(e.message) })
  const promote = useMutation({ mutationFn: ({ id, force }: { id: string; force: boolean }) => api.post(`/api/training/adapters/${id}/promote`, { force, task_class: taskClass || undefined }), onSuccess: onChange, onError: (e) => setErr(e.message) })
  const unpromote = useMutation({ mutationFn: (id: string) => api.del(`/api/training/adapters/${id}/promote`), onSuccess: onChange, onError: (e) => setErr(e.message) })
  const remove = useMutation({ mutationFn: (id: string) => api.del(`/api/training/adapters/${id}`), onSuccess: onChange, onError: (e) => setErr(e.message) })
  const pct = (x: number | null) => (x === null ? '–' : `${Math.round(x * 100)}%`)
  return (
    <section className="space-y-3">
      <SectionHead aside={adapters.length ? `${adapters.length}` : undefined}>adapters</SectionHead>
      {adapters.length > 0 && (
        <label className="flex items-center gap-2 text-xs text-fg-2">
          promote into
          <select value={taskClass} onChange={(e) => setTaskClass(e.target.value)} className={clsx(inputSm, 'w-auto')} aria-label="Task class to promote into" title="Promoting also sends this task class to the adapter first (the routes below); pick none to only enable its endpoint">
            <option value="">no class (enable only)</option>
            {TASK_CLASSES.map((c) => (
              <option key={c} value={c}>{c}</option>
            ))}
          </select>
        </label>
      )}
      <ListGroup empty="No adapters yet: a finished fine-tune job makes one.">
        {adapters.map((a) => (
          <Row
            key={a.id}
            action={
              <span className="flex flex-wrap items-center gap-x-3">
                <a href={`/api/training/adapters/${a.id}/download`} download className={btn.link}>download</a>
                {a.endpoint_id && (a.eval_job?.state === 'queued' || a.eval_job?.state === 'running') ? (
                  <span className="meta">{a.eval_job.state === 'queued' ? 'evaluation queued…' : 'evaluating…'}</span>
                ) : (
                  a.endpoint_id && (
                    <button onClick={() => evaluate.mutate(a.id)} disabled={evaluate.isPending} className={btn.link} title="Run the held-out set through the adapter's endpoint and its base; needs the adapter loaded on the server">
                      {a.eval_job?.state === 'failed' ? 'evaluate again' : 'evaluate'}
                    </button>
                  )
                )}
                {a.promoted ? (
                  <button onClick={() => unpromote.mutate(a.id)} className={btn.link}>unpromote</button>
                ) : (
                  <>
                    <button
                      onClick={async () => {
                        // A passing gate on a few examples is worth a second look before it goes live.
                        if (a.eval && isSmokeEval(a.eval.examples, smoke) && !(await confirmDialog({ title: 'Promote on a smoke result?', body: smokeBody(a.eval.examples, smoke), confirm: 'Promote' }))) return
                        promote.mutate({ id: a.id, force: false })
                      }}
                      className={btn.secondarySm}
                    >
                      promote
                    </button>
                    <button onClick={async () => { if (await confirmDialog({ title: 'Promote without the eval gate?', body: `The adapter goes live even if it scored below its base model.${a.eval && isSmokeEval(a.eval.examples, smoke) ? ` ${smokeBody(a.eval.examples, smoke)}` : ''}`, confirm: 'Promote anyway', danger: true })) promote.mutate({ id: a.id, force: true }) }} className={btn.link}>force</button>
                  </>
                )}
                <button onClick={async () => { if (await confirmDialog({ title: `Delete adapter ${a.name}?`, body: 'The stored adapter files are removed.', confirm: 'Delete adapter', danger: true })) remove.mutate(a.id) }} className={btn.danger}>delete</button>
              </span>
            }
          >
            <span className="flex flex-wrap items-baseline gap-x-2">
              <span className={clsx('section-head border-0 shrink-0', a.promoted ? 'text-fg' : 'text-fg-3')}>{a.promoted ? 'promoted' : 'adapter'}</span>
              {a.name} <span className="meta">on {a.base_model}</span>
            </span>
            <span className="meta block">
              {a.endpoint_id ? `endpoint ${a.endpoint_id} · ` : 'no endpoint (job named no base endpoint) · '}
              {a.eval ? (
                <>
                  {`eval ${pct(a.eval_score)} vs base ${pct(a.baseline_score)} on ${a.eval.examples} (${a.eval.adapter_wins}–${a.eval.baseline_wins}–${a.eval.ties})`}
                  {isSmokeEval(a.eval.examples, smoke) && <span className="section-head border-0 ml-1.5 text-fg-2" title={smokeTitle(a.eval.examples, smoke)}>smoke result</span>}
                  {' · '}
                </>
              ) : (
                'not evaluated · '
              )}
              {a.recipe_id?.Valid ? `from a recipe${a.system_prompt ? `, trained under its system prompt (“${a.system_prompt.slice(0, 60)}${a.system_prompt.length > 60 ? '…' : ''}”)` : ''}${a.rubric ? ', judged by its rubric' : ''} · ` : ''}
              {(a.bytes / 1024 / 1024).toFixed(1)} MB · {ago(a.created_at)}
            </span>
            {a.eval_job?.state === 'failed' && (
              <span className="block whitespace-normal break-words text-xs text-danger">
                The last evaluation failed{a.eval_job.at ? ` ${ago(a.eval_job.at)}` : ''}: {a.eval_job.error}
              </span>
            )}
          </Row>
        ))}
      </ListGroup>
      {err && <Callout kind="error">{err}</Callout>}
    </section>
  )
}

/** Formats a parameter count the way model names do: 494M, 1.5B, 7.6B. */
