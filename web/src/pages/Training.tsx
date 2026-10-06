import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { api, type Adapter, type Dataset, type FinetuneConfig, type FinetuneJob, type FinetuneTarget, type ModelsResponse } from '../api'

type FinetuneConfigTarget = FinetuneConfig['target']
import { SectionHead, PageTitle, Callout, ListGroup, Row, btn, inputSm } from '../components/ui'
import { ago } from './Agents'

const MODES = ['chat', 'code', 'design', 'agent']

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
  const adapters = useQuery({ queryKey: ['training', 'adapters'], queryFn: () => api.get<{ adapters: Adapter[] }>('/api/training/adapters'), refetchInterval: 10000 })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models'), staleTime: 60_000 })
  const refresh = () => qc.invalidateQueries({ queryKey: ['training'] })

  return (
    <div className="flex-1 min-h-0 overflow-y-auto">
      <div className="mx-auto max-w-4xl px-6 py-6 space-y-10">
        <PageTitle>Training</PageTitle>
        <p className="text-sm text-fg-2">
          Rated answers from people who opted in become datasets; a dataset and a base model become an adapter; an adapter that beats its base on the held-out set gets promoted to an endpoint. Operational notes are in <code className="font-mono text-xs">infra/training/README.md</code>.
        </p>
        <Datasets datasets={datasets.data?.datasets ?? []} onChange={refresh} />
        <Jobs jobs={jobs.data?.jobs ?? []} runner={!!jobs.data?.runner} targets={jobs.data?.targets ?? []} datasets={datasets.data?.datasets ?? []} models={models.data} onChange={refresh} />
        <Adapters adapters={adapters.data?.adapters ?? []} onChange={refresh} />
      </div>
    </div>
  )
}

function StatusWord({ s }: { s: string }) {
  const open = s === 'queued' || s === 'building' || s === 'running'
  return <span className={clsx('section-head border-0 shrink-0', open ? 'text-fg' : s === 'failed' ? 'text-danger' : 'text-fg-3')}>{s}</span>
}

function Datasets({ datasets, onChange }: { datasets: Dataset[]; onChange: () => void }) {
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
  return (
    <section className="space-y-3">
      <SectionHead aside={datasets.length ? `${datasets.length}` : undefined}>datasets</SectionHead>
      <ListGroup empty="No datasets yet.">
        {datasets.map((d) => (
          <Row
            key={d.id}
            action={
              <span className="flex items-center gap-3">
                {d.status === 'ready' && (
                  <>
                    <a href={`/api/training/datasets/${d.id}/download`} download className="text-xs text-fg-2 hover:text-fg">train</a>
                    {d.eval_examples > 0 && <a href={`/api/training/datasets/${d.id}/download?split=eval`} download className="text-xs text-fg-2 hover:text-fg">eval</a>}
                  </>
                )}
                <button onClick={() => remove.mutate(d.id)} className={btn.danger}>remove</button>
              </span>
            }
          >
            <span className="flex items-center gap-2">
              <StatusWord s={d.status} />
              {d.name}
            </span>
            <span className="meta block">
              {d.status === 'ready' ? `${d.examples} examples, ${d.eval_examples} held out · ${(d.bytes / 1024).toFixed(0)} KB · ` : ''}
              {d.filters.modes?.length ? d.filters.modes.join(', ') + ' · ' : ''}
              {d.filters.min_rating === 1 ? 'upvoted only' : d.filters.min_rating === -1 ? 'all ratings' : 'upvoted or unrated'} · {ago(d.created_at)}
              {d.error ? ` · ${d.error}` : ''}
            </span>
          </Row>
        ))}
      </ListGroup>
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
        config: { epochs, rank, learning_rate: Number(lr) || undefined, target: (target || undefined) as FinetuneConfigTarget },
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
          No fine-tune runner on the worker: set <code className="font-mono text-xs">WS_FINETUNE_IMAGE</code> where Docker and a GPU are, or <code className="font-mono text-xs">WS_FINETUNE_TEMPLATE</code> to a trainer rental template to train on a rented GPU (see <code className="font-mono text-xs">infra/training</code>). Datasets and ratings work without it.
        </Callout>
      )}
      <ListGroup empty="No jobs yet.">
        {jobs.map((j) => (
          <Row
            key={j.id}
            action={
              <span className="flex items-center gap-3">
                <button onClick={() => setShowLog(showLog === j.id ? null : j.id)} className="text-xs text-fg-2 hover:text-fg">
                  {showLog === j.id ? 'hide log' : 'log'}
                </button>
                {(j.status === 'queued' || j.status === 'running') && (
                  <button onClick={() => cancel.mutate(j.id)} className={btn.danger}>cancel</button>
                )}
              </span>
            }
          >
            <span className="flex items-center gap-2">
              <StatusWord s={j.status} />
              {j.adapter_name} <span className="meta">on {j.base_model}</span>
            </span>
            <span className="meta block">
              {j.status === 'running' ? `${Math.round(j.progress * 100)}% · ` : ''}
              {j.config.epochs ?? 2} epochs, rank {j.config.rank ?? 16}{j.config.target === 'rental' ? ' · rented GPU' : ''} · {ago(j.created_at)}
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
          <input value={baseModel} onChange={(e) => setBaseModel(e.target.value)} placeholder="base model id the trainer loads (defaults to the endpoint's)" className={clsx(inputSm, 'flex-1 min-w-64 font-mono')} />
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

function Adapters({ adapters, onChange }: { adapters: Adapter[]; onChange: () => void }) {
  const [err, setErr] = useState('')
  const evaluate = useMutation({ mutationFn: (id: string) => api.post(`/api/training/adapters/${id}/evaluate`, {}), onSuccess: onChange, onError: (e) => setErr(e.message) })
  const promote = useMutation({ mutationFn: ({ id, force }: { id: string; force: boolean }) => api.post(`/api/training/adapters/${id}/promote`, { force }), onSuccess: onChange, onError: (e) => setErr(e.message) })
  const unpromote = useMutation({ mutationFn: (id: string) => api.del(`/api/training/adapters/${id}/promote`), onSuccess: onChange, onError: (e) => setErr(e.message) })
  const remove = useMutation({ mutationFn: (id: string) => api.del(`/api/training/adapters/${id}`), onSuccess: onChange, onError: (e) => setErr(e.message) })
  const pct = (x: number | null) => (x === null ? '–' : `${Math.round(x * 100)}%`)
  return (
    <section className="space-y-3">
      <SectionHead aside={adapters.length ? `${adapters.length}` : undefined}>adapters</SectionHead>
      <ListGroup empty="No adapters yet: a finished fine-tune job makes one.">
        {adapters.map((a) => (
          <Row
            key={a.id}
            action={
              <span className="flex items-center gap-3">
                <a href={`/api/training/adapters/${a.id}/download`} download className="text-xs text-fg-2 hover:text-fg">download</a>
                {a.endpoint_id && (
                  <button onClick={() => evaluate.mutate(a.id)} disabled={evaluate.isPending} className="text-xs text-fg-2 hover:text-fg" title="Run the held-out set through the adapter's endpoint and its base; needs the adapter loaded on the server">
                    evaluate
                  </button>
                )}
                {a.promoted ? (
                  <button onClick={() => unpromote.mutate(a.id)} className="text-xs text-fg-2 hover:text-fg">unpromote</button>
                ) : (
                  <>
                    <button onClick={() => promote.mutate({ id: a.id, force: false })} className={btn.secondarySm}>promote</button>
                    <button onClick={() => { if (confirm('Promote without the eval gate?')) promote.mutate({ id: a.id, force: true }) }} className="text-xs text-fg-2 hover:text-fg">force</button>
                  </>
                )}
                <button onClick={() => { if (confirm(`Delete adapter ${a.name}?`)) remove.mutate(a.id) }} className={btn.danger}>delete</button>
              </span>
            }
          >
            <span className="flex items-center gap-2">
              <span className={clsx('section-head border-0 shrink-0', a.promoted ? 'text-fg' : 'text-fg-3')}>{a.promoted ? 'promoted' : 'adapter'}</span>
              {a.name} <span className="meta">on {a.base_model}</span>
            </span>
            <span className="meta block">
              {a.endpoint_id ? `endpoint ${a.endpoint_id} · ` : 'no endpoint (job named no base endpoint) · '}
              {a.eval ? `eval ${pct(a.eval_score)} vs base ${pct(a.baseline_score)} on ${a.eval.examples} (${a.eval.adapter_wins}–${a.eval.baseline_wins}–${a.eval.ties}) · ` : 'not evaluated · '}
              {(a.bytes / 1024 / 1024).toFixed(1)} MB · {ago(a.created_at)}
            </span>
          </Row>
        ))}
      </ListGroup>
      {err && <Callout kind="error">{err}</Callout>}
    </section>
  )
}
