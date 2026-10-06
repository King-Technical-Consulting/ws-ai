import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { api, type MediaJob, type MediaModel, type ModelsResponse, type Project } from '../api'
import { SectionHead, PageTitle, Callout, btn, inputSm } from '../components/ui'

/**
 * Images (PLAN M6): a per-project gallery of media jobs. A job names a
 * model (or auto), size, count and quality; the worker runs it on a media
 * endpoint through the gateway and the outputs come back as attachments.
 * Cards poll while a job is queued or running, so a reload mid-job is fine.
 */
export default function Images() {
  const qc = useQueryClient()
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models') })
  const media = useMemo(() => (models.data?.media ?? []).filter((m) => m.image), [models.data])

  const [projectID, setProjectID] = useState<string>('')
  useEffect(() => {
    if (projectID || !projects.data?.length) return
    const imgs = projects.data.find((p) => p.kind === 'images')
    setProjectID((imgs ?? projects.data[0]).id)
  }, [projects.data, projectID])

  const jobs = useQuery({
    queryKey: ['media', projectID],
    queryFn: () => api.get<{ jobs: MediaJob[] }>(`/api/projects/${projectID}/media?kind=image`),
    enabled: !!projectID,
    refetchInterval: (q) => (q.state.data?.jobs.some((j) => j.status === 'queued' || j.status === 'running') ? 2000 : false),
  })

  const newProject = useMutation({
    mutationFn: () => api.post<Project>('/api/projects', { name: 'Images', kind: 'images' }),
    onSuccess: async (p) => {
      await qc.invalidateQueries({ queryKey: ['projects'] })
      setProjectID(p.id)
    },
  })

  const [prompt, setPrompt] = useState('')
  const [model, setModel] = useState('auto')
  const [size, setSize] = useState('')
  const [n, setN] = useState(1)
  const [quality, setQuality] = useState('')
  const chosen = media.find((m) => m.id === model) ?? media[0]
  const sizes = chosen?.sizes ?? []
  const maxImages = Math.min(4, chosen?.max_images ?? 1)
  const estimate = chosen && !chosen.local ? chosen.per_image * n : 0

  const create = useMutation({
    mutationFn: () =>
      api.post<MediaJob>(`/api/projects/${projectID}/media`, {
        kind: 'image',
        prompt,
        model: model === 'auto' ? '' : model,
        size: size || undefined,
        n,
        quality: quality || undefined,
      }),
    onSuccess: (job) => {
      qc.setQueryData<{ jobs: MediaJob[] }>(['media', projectID], (cur) => ({ jobs: [job, ...(cur?.jobs ?? [])] }))
      setPrompt('')
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/media/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['media', projectID] }),
  })

  const list = jobs.data?.jobs ?? []

  return (
    <div className="flex-1 min-h-0 overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 py-6 space-y-6">
        <div className="flex items-baseline justify-between gap-4">
          <PageTitle>Images</PageTitle>
          {projects.data && projects.data.length > 0 && (
            <select value={projectID} onChange={(e) => setProjectID(e.target.value)} className={`${inputSm} max-w-[14rem]`} aria-label="Project">
              {projects.data.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          )}
        </div>

        {models.isSuccess && media.length === 0 && (
          <Callout kind="note">
            No image model is configured. Add an endpoint with <code className="font-mono text-xs">capabilities.media</code> in <code className="font-mono text-xs">config/endpoints.yaml</code> and set its provider's key (for OpenAI, <code className="font-mono text-xs">OPENAI_API_KEY</code>).
          </Callout>
        )}
        {projects.isSuccess && projects.data.length === 0 && (
          <div className="flex items-center gap-3">
            <p className="meta">Images live in a project.</p>
            <button onClick={() => newProject.mutate()} disabled={newProject.isPending} className={btn.secondarySm}>
              Create an Images project
            </button>
          </div>
        )}

        <form
          className="space-y-3 rounded-2xl border border-line bg-bg-2 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (prompt.trim() && projectID) create.mutate()
          }}
        >
          <textarea
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="Describe the image…"
            rows={3}
            className="w-full resize-none bg-transparent outline-none reading"
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && prompt.trim() && projectID) create.mutate()
            }}
          />
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <select value={model} onChange={(e) => { setModel(e.target.value); setSize('') }} className={`${inputSm} w-auto max-w-[16rem]`} aria-label="Model">
              <option value="auto">auto</option>
              {media.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.display_name}
                  {m.local ? ' · local' : m.per_image ? ` · $${m.per_image.toFixed(2)}/image` : ''}
                </option>
              ))}
            </select>
            {sizes.length > 0 && (
              <select value={size} onChange={(e) => setSize(e.target.value)} className={`${inputSm} w-auto`} aria-label="Size">
                <option value="">default size</option>
                {sizes.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            )}
            <select value={n} onChange={(e) => setN(Number(e.target.value))} className={`${inputSm} w-auto`} aria-label="Count">
              {Array.from({ length: maxImages }, (_, i) => i + 1).map((k) => (
                <option key={k} value={k}>
                  {k} image{k === 1 ? '' : 's'}
                </option>
              ))}
            </select>
            {chosen?.engine === 'openai_images' && (
              <select value={quality} onChange={(e) => setQuality(e.target.value)} className={`${inputSm} w-auto`} aria-label="Quality">
                <option value="">default quality</option>
                <option value="low">low</option>
                <option value="medium">medium</option>
                <option value="high">high</option>
              </select>
            )}
            <span className="meta ml-auto">
              {chosen ? (chosen.local ? 'free, local' : `about $${estimate.toFixed(2)}`) : ''}
            </span>
            <button type="submit" disabled={create.isPending || !prompt.trim() || !projectID || media.length === 0} className={btn.primarySm}>
              {create.isPending ? 'Starting…' : 'Generate'}
            </button>
          </div>
          {create.error && <Callout kind="error">{(create.error as Error).message}</Callout>}
        </form>

        <section className="space-y-3">
          <SectionHead aside={list.length ? `${list.length} job${list.length === 1 ? '' : 's'}` : undefined}>gallery</SectionHead>
          {jobs.error && <Callout kind="error">{(jobs.error as Error).message}</Callout>}
          {jobs.isSuccess && list.length === 0 && <p className="meta">Nothing generated yet.</p>}
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {list.map((j) => (
              <JobCard key={j.id} job={j} models={media} onRemove={() => remove.mutate(j.id)} />
            ))}
          </div>
        </section>
      </div>
    </div>
  )
}

function JobCard({ job, models, onRemove }: { job: MediaJob; models: MediaModel[]; onRemove: () => void }) {
  const open = job.status === 'queued' || job.status === 'running'
  const name = models.find((m) => m.id === job.endpoint_id)?.display_name ?? job.endpoint_id ?? job.selector
  return (
    <article className="rounded-lg border border-line overflow-hidden flex flex-col">
      <div className={clsx('bg-bg-2 grid place-items-center', job.outputs.length > 1 ? 'grid-cols-2 gap-px' : '', 'min-h-40')}>
        {job.outputs.map((a) => (
          <a key={a.id} href={a.url} target="_blank" rel="noopener" className="block w-full">
            <img src={a.url} alt={job.inputs.prompt} width={a.width ?? undefined} height={a.height ?? undefined} className="w-full h-auto block" loading="lazy" />
          </a>
        ))}
        {open && (
          <div className="p-6 text-center">
            <span className="thinking">{job.status === 'queued' ? 'Waiting for the worker…' : 'Generating…'}</span>
            {job.progress > 0 && job.progress < 1 && <div className="meta mt-1 tnum">{Math.round(job.progress * 100)}%</div>}
          </div>
        )}
        {job.status === 'failed' && (
          <div className="p-4 w-full">
            <Callout kind="error">{job.error ?? 'failed'}</Callout>
          </div>
        )}
        {job.status === 'cancelled' && <span className="meta p-6">cancelled</span>}
      </div>
      <div className="p-3 space-y-1">
        <p className="reading-tight text-[15px] line-clamp-3" title={job.inputs.prompt}>
          {job.inputs.prompt}
        </p>
        <div className="flex items-baseline justify-between gap-2">
          <span className="meta truncate">
            {name}
            {job.inputs.size ? ` · ${job.inputs.size}` : ''}
            {job.status === 'done' ? ` · $${job.cost_usd.toFixed(3)}` : job.inputs.estimate_usd ? ` · about $${job.inputs.estimate_usd.toFixed(2)}` : ''}
            {` · ${ago(job.created_at)}`}
          </span>
          {job.status !== 'running' && (
            <button onClick={onRemove} className={btn.danger}>
              {job.status === 'queued' ? 'cancel' : 'remove'}
            </button>
          )}
        </div>
      </div>
    </article>
  )
}

function ago(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  if (!Number.isFinite(ms) || ms < 0) return ''
  const m = Math.floor(ms / 60_000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}
