import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { api, type Attachment, type Conversation, type MediaJob, type MediaModel, type ModelsResponse, type Project } from '../api'
import { SectionHead, PageTitle, Callout, btn, inputSm } from '../components/ui'

type Kind = 'image' | 'video'

/**
 * Media (PLAN M6): a per-project gallery of image and video jobs. A job
 * names a model (or auto), a size, a count or a length, and optionally a
 * source image (then it is an edit, or image to video); the worker runs it
 * on a media endpoint through the gateway and the outputs come back as
 * attachments. Cards poll while a job is queued or running, so a reload
 * mid-job is fine.
 */
export default function Media() {
  const qc = useQueryClient()
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models') })
  const all = useMemo(() => models.data?.media ?? [], [models.data])

  const [kind, setKind] = useState<Kind>('image')
  const [source, setSource] = useState<Attachment | null>(null)
  // Which models can take this request: text to image, edit, text to
  // video or image to video.
  const media = useMemo(
    () => all.filter((m) => (kind === 'image' ? (source ? m.image_edit : m.image) : source ? m.image_to_video : m.video)),
    [all, kind, source],
  )
  // Every endpoint that could take this request is down (its server does not
  // answer): the router would refuse the job, so say so before it is sent.
  const allDown = media.length > 0 && media.every((m) => m.health === 'down')
  const canEdit = all.some((m) => m.image_edit)
  const canAnimate = all.some((m) => m.image_to_video)
  const canUpscale = all.some((m) => m.upscale)
  const hasVideo = all.some((m) => m.video || m.image_to_video)
  // An edit can carry a mask: an image whose transparent area marks where
  // the change applies.
  const [mask, setMask] = useState<Attachment | null>(null)
  const nav = useNavigate()

  const [projectID, setProjectID] = useState<string>('')
  useEffect(() => {
    if (projectID || !projects.data?.length) return
    const imgs = projects.data.find((p) => p.kind === 'images')
    setProjectID((imgs ?? projects.data[0]).id)
  }, [projects.data, projectID])

  const jobs = useQuery({
    queryKey: ['media', projectID],
    queryFn: () => api.get<{ jobs: MediaJob[] }>(`/api/projects/${projectID}/media?limit=120`),
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
  const [seconds, setSeconds] = useState(0)
  const chosen = media.find((m) => m.id === model) ?? media[0]
  const sizes = chosen?.sizes ?? []
  const maxImages = Math.min(4, chosen?.max_images ?? 1)
  const lengths = chosen?.seconds ?? []
  const secs = seconds || lengths[0] || Math.min(5, chosen?.max_seconds || 5)
  const estimate = chosen && !chosen.local ? (kind === 'video' ? chosen.per_second * secs : chosen.per_image * n) : 0

  const create = useMutation({
    mutationFn: () =>
      api.post<MediaJob>(`/api/projects/${projectID}/media`, {
        kind,
        prompt,
        model: model === 'auto' ? '' : model,
        size: size || undefined,
        n: kind === 'image' ? n : 1,
        quality: kind === 'image' && quality ? quality : undefined,
        seconds: kind === 'video' ? secs : undefined,
        source_attachment_id: source?.id || undefined,
        mask_attachment_id: kind === 'image' && source && mask ? mask.id : undefined,
      }),
    onSuccess: (job) => {
      qc.setQueryData<{ jobs: MediaJob[] }>(['media', projectID], (cur) => ({ jobs: [job, ...(cur?.jobs ?? [])] }))
      setPrompt('')
    },
  })

  // Upscale a finished image straight from its card: a source, no prompt.
  const upscale = useMutation({
    mutationFn: (att: Attachment) => api.post<MediaJob>(`/api/projects/${projectID}/media`, { kind: 'upscale', prompt: '', source_attachment_id: att.id, scale: 2 }),
    onSuccess: (job) => {
      qc.setQueryData<{ jobs: MediaJob[] }>(['media', projectID], (cur) => ({ jobs: [job, ...(cur?.jobs ?? [])] }))
    },
  })

  // Use in chat: a new conversation in this project with the image
  // attached to the first message (the chat page reads ?attach=).
  // Use in design: the same, with the conversation created in design mode
  // (the server takes `mode` in any project the caller can reach).
  const useInChat = useMutation({
    mutationFn: ({ att, mode }: { att: Attachment; mode?: 'design' }) =>
      api.post<Conversation>(`/api/projects/${projectID}/conversations`, mode ? { mode } : {}).then((c) => ({ c, att })),
    onSuccess: ({ c, att }) => {
      qc.invalidateQueries({ queryKey: ['convs'] })
      nav(`/c/${c.id}?attach=${att.id}`)
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/media/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['media', projectID] }),
  })

  const fileInput = useRef<HTMLInputElement>(null)
  const maskInput = useRef<HTMLInputElement>(null)
  const upload = useMutation({
    mutationFn: ({ file }: { file: File; as: 'source' | 'mask' }) => {
      const fd = new FormData()
      fd.append('file', file)
      return api.postRaw<Attachment>(`/api/projects/${projectID}/attachments`, fd)
    },
    onSuccess: (att, v) => (v.as === 'mask' ? setMask(att) : setSource(att)),
  })

  const useAsSource = (att: Attachment, k: Kind) => {
    setSource(att)
    setMask(null)
    setKind(k)
    setModel('auto')
    setSize('')
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }

  const list = (jobs.data?.jobs ?? []).filter((j) => (kind === 'video' ? j.kind === 'video' : j.kind !== 'video'))
  const ready = prompt.trim() && projectID && media.length > 0 && !allDown

  return (
    <div className="flex-1 min-h-0 overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 py-6 space-y-6">
        <div className="flex items-baseline justify-between gap-4">
          <div className="flex items-baseline gap-4">
            <PageTitle>Media</PageTitle>
            <div className="flex gap-3 text-sm">
              <Tab active={kind === 'image'} onClick={() => { setKind('image'); setModel('auto'); setSize('') }}>images</Tab>
              <Tab active={kind === 'video'} onClick={() => { setKind('video'); setModel('auto'); setSize('') }} disabled={!hasVideo} title={hasVideo ? undefined : 'No video model is configured.'}>
                video
              </Tab>
            </div>
          </div>
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

        {models.isSuccess && all.length === 0 && (
          <Callout kind="note">
            <p className="reading">{noEngineNote(kind)}</p>
          </Callout>
        )}
        {models.isSuccess && allDown && (
          <Callout kind="note">
            <p className="reading">{downNote(kind, media.map((m) => m.id).sort())}</p>
          </Callout>
        )}
        {models.isSuccess && all.length > 0 && media.length === 0 && (
          <Callout kind="note">
            {source
              ? kind === 'image'
                ? 'No configured model edits images. Clear the source to generate from the prompt alone.'
                : 'No configured model animates an image. Clear the source for text to video.'
              : kind === 'image'
                ? noEngineNote('image')
                : 'No configured model makes video from text.'}
          </Callout>
        )}
        {projects.isSuccess && projects.data.length === 0 && (
          <div className="flex items-center gap-3">
            <p className="meta">Media lives in a project.</p>
            <button onClick={() => newProject.mutate()} disabled={newProject.isPending} className={btn.secondarySm}>
              Create an Images project
            </button>
          </div>
        )}

        <form
          className="space-y-3 rounded-2xl border border-line bg-bg-2 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (ready) create.mutate()
          }}
        >
          {source && (
            <div className="flex items-center gap-3 text-sm">
              <img src={source.url} alt="" className="h-14 w-14 rounded-md object-cover border border-line" />
              <span className="text-fg-2">
                {kind === 'image' ? 'Editing' : 'Animating'} <span className="font-mono text-xs">{source.filename || source.id.slice(0, 8)}</span>
                {source.width && source.height ? <span className="meta"> · {source.width}×{source.height}</span> : null}
              </span>
              {kind === 'image' && mask && (
                <span className="flex items-center gap-2 text-fg-2">
                  <img src={mask.url} alt="" className="h-10 w-10 rounded-md object-cover border border-line" />
                  mask
                  <button type="button" onClick={() => setMask(null)} className="text-xs text-fg-2 hover:text-fg">
                    clear
                  </button>
                </span>
              )}
              {kind === 'image' && !mask && (
                <>
                  <input ref={maskInput} type="file" accept="image/*" className="hidden" onChange={(e) => { const f = e.target.files?.[0]; if (f) upload.mutate({ file: f, as: 'mask' }); e.target.value = '' }} />
                  <button type="button" onClick={() => maskInput.current?.click()} disabled={upload.isPending} className="text-xs text-fg-2 hover:text-fg" title="A PNG whose transparent area marks where the edit applies">
                    add a mask…
                  </button>
                </>
              )}
              <button type="button" onClick={() => { setSource(null); setMask(null) }} className="text-xs text-fg-2 hover:text-fg ml-auto">
                clear source
              </button>
            </div>
          )}
          <textarea
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder={source ? (kind === 'image' ? 'Describe the change…' : 'Describe the motion…') : kind === 'image' ? 'Describe the image…' : 'Describe the video: scene, motion, camera…'}
            rows={3}
            className="w-full resize-none bg-transparent outline-none reading"
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && ready) create.mutate()
            }}
          />
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <select value={model} onChange={(e) => { setModel(e.target.value); setSize(''); setSeconds(0) }} className={`${inputSm} w-auto max-w-[16rem]`} aria-label="Model">
              <option value="auto">auto</option>
              {media.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.display_name}
                  {m.local ? ' · local' : kind === 'video' && m.per_second ? ` · $${m.per_second.toFixed(2)}/s` : m.per_image ? ` · $${m.per_image.toFixed(2)}/image` : ''}
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
            {kind === 'image' && (
              <select value={n} onChange={(e) => setN(Number(e.target.value))} className={`${inputSm} w-auto`} aria-label="Count">
                {Array.from({ length: maxImages }, (_, i) => i + 1).map((k) => (
                  <option key={k} value={k}>
                    {k} image{k === 1 ? '' : 's'}
                  </option>
                ))}
              </select>
            )}
            {kind === 'image' && chosen?.engine === 'openai_images' && (
              <select value={quality} onChange={(e) => setQuality(e.target.value)} className={`${inputSm} w-auto`} aria-label="Quality">
                <option value="">default quality</option>
                <option value="low">low</option>
                <option value="medium">medium</option>
                <option value="high">high</option>
              </select>
            )}
            {kind === 'video' &&
              (lengths.length > 0 ? (
                <select value={secs} onChange={(e) => setSeconds(Number(e.target.value))} className={`${inputSm} w-auto`} aria-label="Length">
                  {lengths.map((s) => (
                    <option key={s} value={s}>
                      {s} s
                    </option>
                  ))}
                </select>
              ) : (
                <label className="flex items-center gap-1 text-xs text-fg-2">
                  <input type="number" min={1} max={chosen?.max_seconds || 60} value={secs} onChange={(e) => setSeconds(Number(e.target.value))} className={`${inputSm} w-16 tnum`} aria-label="Length in seconds" /> s
                </label>
              ))}
            {!source && (canEdit || canAnimate) && projectID && (
              <>
                <input ref={fileInput} type="file" accept="image/*" className="hidden" onChange={(e) => { const f = e.target.files?.[0]; if (f) upload.mutate({ file: f, as: 'source' }); e.target.value = '' }} />
                <button type="button" onClick={() => fileInput.current?.click()} disabled={upload.isPending} className="text-xs text-fg-2 hover:text-fg">
                  {upload.isPending ? 'uploading…' : kind === 'image' ? 'edit a photo…' : 'animate a photo…'}
                </button>
              </>
            )}
            <span className="meta ml-auto">
              {chosen ? (chosen.local ? 'free, local' : `about $${estimate.toFixed(2)}`) : ''}
              {kind === 'video' && chosen ? ' · takes minutes' : ''}
            </span>
            <button type="submit" disabled={create.isPending || !ready} className={btn.primarySm}>
              {create.isPending ? 'Starting…' : source ? (kind === 'image' ? 'Edit' : 'Animate') : kind === 'image' ? 'Generate' : 'Render'}
            </button>
          </div>
          {create.error && <Callout kind="error">{(create.error as Error).message}</Callout>}
          {upload.error && <Callout kind="error">{(upload.error as Error).message}</Callout>}
          {upscale.error && <Callout kind="error">{(upscale.error as Error).message}</Callout>}
          {useInChat.error && <Callout kind="error">{(useInChat.error as Error).message}</Callout>}
        </form>

        <section className="space-y-3">
          <SectionHead aside={list.length ? `${list.length} job${list.length === 1 ? '' : 's'}` : undefined}>gallery</SectionHead>
          {jobs.error && <Callout kind="error">{(jobs.error as Error).message}</Callout>}
          {jobs.isSuccess && list.length === 0 && <p className="meta">Nothing {kind === 'video' ? 'rendered' : 'generated'} yet.</p>}
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {list.map((j) => (
              <JobCard
                key={j.id}
                job={j}
                models={all}
                canEdit={canEdit}
                canAnimate={canAnimate}
                canUpscale={canUpscale}
                onSource={useAsSource}
                onUpscale={(a) => upscale.mutate(a)}
                onChat={(a) => useInChat.mutate({ att: a })}
                onDesign={(a) => useInChat.mutate({ att: a, mode: 'design' })}
                onRemove={() => remove.mutate(j.id)}
              />
            ))}
          </div>
        </section>
      </div>
    </div>
  )
}

// The router's own reason (gateway.NoMediaError), word for word, so the page
// and a failed job say the same thing.
function noEngineNote(kind: Kind): string {
  return `No ${kind} endpoint is configured; the owner sets COMFYUI_URL or a media provider key, or enables one under Admin, endpoints.`
}

// The router's wording for endpoints that are down (gateway.NoMediaError), less
// the health error, which /api/models does not carry.
function downNote(kind: Kind, ids: string[]): string {
  if (ids.length === 1) return `The ${kind} endpoint ${ids[0]} is down; the owner checks the server it points at (COMFYUI_URL) and Admin, endpoints.`
  return `Every ${kind} endpoint is down (${ids.join(', ')}); the owner checks the servers they point at and Admin, endpoints.`
}

function Tab({ active, onClick, disabled, title, children }: { active: boolean; onClick: () => void; disabled?: boolean; title?: string; children: React.ReactNode }) {
  return (
    <button type="button" onClick={onClick} disabled={disabled} title={title} className={clsx('section-head border-0 px-0 min-h-10 md:min-h-0', active ? 'text-fg' : 'text-fg-3 hover:text-fg', disabled && 'opacity-50 hover:text-fg-3')}>
      {children}
    </button>
  )
}

function JobCard({
  job,
  models,
  canEdit,
  canAnimate,
  canUpscale,
  onSource,
  onUpscale,
  onChat,
  onDesign,
  onRemove,
}: {
  job: MediaJob
  models: MediaModel[]
  canEdit: boolean
  canAnimate: boolean
  canUpscale: boolean
  onSource: (a: Attachment, k: Kind) => void
  onUpscale: (a: Attachment) => void
  onChat: (a: Attachment) => void
  onDesign: (a: Attachment) => void
  onRemove: () => void
}) {
  const open = job.status === 'queued' || job.status === 'running'
  const name = models.find((m) => m.id === job.endpoint_id)?.display_name ?? job.endpoint_id ?? job.selector
  const video = job.kind === 'video'
  return (
    <article className="rounded-lg border border-line overflow-hidden flex flex-col">
      <div className={clsx('bg-bg-2 grid place-items-center', job.outputs.length > 1 ? 'grid-cols-2 gap-px' : '', 'min-h-40')}>
        {job.outputs.map((a) =>
          a.mime.startsWith('video/') ? (
            <video key={a.id} src={a.url} controls playsInline preload="metadata" width={a.width ?? undefined} height={a.height ?? undefined} className="w-full h-auto block" />
          ) : (
            <a key={a.id} href={a.url} target="_blank" rel="noopener" className="block w-full">
              <img src={a.url} alt={job.inputs.prompt} width={a.width ?? undefined} height={a.height ?? undefined} className="w-full h-auto block" loading="lazy" />
            </a>
          ),
        )}
        {open && (
          <div className="p-6 text-center">
            <span className="thinking">{job.status === 'queued' ? 'Waiting for the worker…' : video ? 'Rendering…' : job.kind === 'edit' ? 'Editing…' : job.kind === 'upscale' ? 'Upscaling…' : 'Generating…'}</span>
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
          {job.inputs.prompt || (job.kind === 'upscale' ? `Upscaled ${job.inputs.scale ?? 2}×` : '')}
        </p>
        <div className="flex items-baseline justify-between gap-2">
          <span className="meta truncate">
            {job.kind === 'edit' ? (job.inputs.mask_attachment_id ? 'masked edit · ' : 'edit · ') : job.kind === 'upscale' ? 'upscale · ' : job.inputs.source_attachment_id ? 'from image · ' : ''}
            {name}
            {job.inputs.size ? ` · ${job.inputs.size}` : ''}
            {video && job.inputs.seconds ? ` · ${job.inputs.seconds} s` : ''}
            {job.status === 'done' ? ` · $${job.cost_usd.toFixed(3)}` : job.inputs.estimate_usd ? ` · about $${job.inputs.estimate_usd.toFixed(2)}` : ''}
            {` · ${ago(job.created_at)}`}
          </span>
          <span className="flex items-center gap-3 shrink-0">
            {!video && job.status === 'done' && job.outputs[0] && canEdit && (
              <button onClick={() => onSource(job.outputs[0], 'image')} className="text-xs text-fg-2 hover:text-fg">
                edit
              </button>
            )}
            {!video && job.status === 'done' && job.outputs[0] && canAnimate && (
              <button onClick={() => onSource(job.outputs[0], 'video')} className="text-xs text-fg-2 hover:text-fg">
                animate
              </button>
            )}
            {!video && job.status === 'done' && job.outputs[0] && canUpscale && job.kind !== 'upscale' && (
              <button onClick={() => onUpscale(job.outputs[0])} className="text-xs text-fg-2 hover:text-fg" title="Upscale 2× (a new job)">
                upscale
              </button>
            )}
            {!video && job.status === 'done' && job.outputs[0] && (
              <button onClick={() => onChat(job.outputs[0])} className="text-xs text-fg-2 hover:text-fg" title="Start a conversation with this image attached">
                use in chat
              </button>
            )}
            {!video && job.status === 'done' && job.outputs[0] && (
              <button onClick={() => onDesign(job.outputs[0])} className="text-xs text-fg-2 hover:text-fg" title="Start a design conversation with this image attached">
                use in design
              </button>
            )}
            {job.status !== 'running' && (
              <button onClick={onRemove} className={btn.danger}>
                {job.status === 'queued' ? 'cancel' : 'remove'}
              </button>
            )}
          </span>
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
