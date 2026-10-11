import { useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type ModelsResponse, type Project } from '../api'
import { SectionHead, PageTitle, Callout, ListGroup, btn, inputSm } from '../components/ui'

type Schedule = {
  id: string
  project_id: string
  title: string
  prompt: string
  model: string
  kind: 'once' | 'cron'
  cron: string
  timezone: string
  run_at: string | null
  enabled: boolean
  allow_web_fetch: boolean
  notify_email: boolean
  reuse_conversation: boolean
  conversation_id: string | null
  next_run_at: string | null
  last_run_at: string | null
  last_status: string
  last_error: string
}
type Run = { id: string; conversation_id: string | null; status: string; error: string; started_at: string; ended_at: string | null }

type Repeat = 'daily' | 'weekdays' | 'weekly' | 'custom'
const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']

function cronFor(repeat: Repeat, time: string, day: number, custom: string): string {
  const [h, m] = time.split(':').map((x) => parseInt(x, 10) || 0)
  switch (repeat) {
    case 'daily': return `${m} ${h} * * *`
    case 'weekdays': return `${m} ${h} * * 1-5`
    case 'weekly': return `${m} ${h} * * ${day}`
    default: return custom
  }
}

const zone = () => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}

function when(s: Schedule) {
  if (s.kind === 'once') return s.run_at ? `once, ${new Date(s.run_at).toLocaleString()}` : 'once'
  return `${s.cron} (${s.timezone})`
}

export default function Scheduled() {
  const qc = useQueryClient()
  const list = useQuery({ queryKey: ['scheduled'], queryFn: () => api.get<{ schedules: Schedule[] }>('/api/scheduled') })
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models') })
  const refresh = () => qc.invalidateQueries({ queryKey: ['scheduled'] })
  const [error, setError] = useState('')
  const fail = (e: unknown) => setError(e instanceof Error ? e.message : 'Something went wrong.')

  const [title, setTitle] = useState('')
  // /schedule in the composer opens this page with the text prefilled.
  const [params] = useSearchParams()
  const [prompt, setPrompt] = useState(params.get('prompt') ?? '')
  const [kind, setKind] = useState<'once' | 'cron'>('once')
  const [runAt, setRunAt] = useState('')
  const [repeat, setRepeat] = useState<Repeat>('daily')
  const [time, setTime] = useState('09:00')
  const [day, setDay] = useState(1)
  const [custom, setCustom] = useState('0 9 * * *')
  const [tz, setTz] = useState(zone())
  const [project, setProject] = useState('')
  const [model, setModel] = useState('auto')
  const [webFetch, setWebFetch] = useState(false)
  const [email, setEmail] = useState(false)
  const [reuse, setReuse] = useState(false)

  const chatProjects = (projects.data ?? []).filter((p) => p.kind === 'chat')
  const projectID = project || chatProjects[0]?.id || ''
  const selectors = ['auto', ...Object.keys(models.data?.aliases ?? {})]

  const create = useMutation({
    mutationFn: () =>
      api.post('/api/scheduled', {
        project_id: projectID,
        title,
        prompt,
        model,
        kind,
        cron: kind === 'cron' ? cronFor(repeat, time, day, custom) : '',
        timezone: tz,
        run_at: kind === 'once' && runAt ? new Date(runAt).toISOString() : null,
        enabled: true,
        allow_web_fetch: webFetch,
        notify_email: email,
        reuse_conversation: reuse,
      }),
    onSuccess: () => {
      setPrompt('')
      setTitle('')
      setError('')
      refresh()
    },
    onError: fail,
  })

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-2xl p-6 space-y-10">
        <div>
          <PageTitle>Scheduled</PageTitle>
          <p className="tagline mt-1">Prompts that run on their own, once or on a repeat, and leave the answer in a conversation.</p>
        </div>
        {error && <Callout kind="error">{error}</Callout>}

        <section className="space-y-3">
          <SectionHead aside={`${list.data?.schedules.length ?? 0} of 20`}>your schedules</SectionHead>
          <ListGroup empty="Nothing scheduled yet.">
            {(list.data?.schedules ?? []).map((s) => (
              <ScheduleRow key={s.id} s={s} onChange={refresh} onError={fail} />
            ))}
          </ListGroup>
        </section>

        <section className="space-y-3">
          <SectionHead>new</SectionHead>
          <form
            className="space-y-3 text-sm"
            onSubmit={(e) => {
              e.preventDefault()
              create.mutate()
            }}
          >
            <div>
              <label htmlFor="sch-title" className="meta block">Name (optional)</label>
              <input id="sch-title" value={title} onChange={(e) => setTitle(e.target.value)} className={inputSm} maxLength={120} />
            </div>
            <div>
              <label htmlFor="sch-prompt" className="meta block">Prompt</label>
              <textarea id="sch-prompt" value={prompt} onChange={(e) => setPrompt(e.target.value)} rows={4} className={inputSm + ' reading'} placeholder="Summarise what changed in the repo this week" />
            </div>
            <div className="flex flex-wrap gap-3 items-end">
              <div>
                <label htmlFor="sch-kind" className="meta block">When</label>
                <select id="sch-kind" value={kind} onChange={(e) => setKind(e.target.value as 'once' | 'cron')} className={inputSm}>
                  <option value="once">Once</option>
                  <option value="cron">On a repeat</option>
                </select>
              </div>
              {kind === 'once' ? (
                <div>
                  <label htmlFor="sch-at" className="meta block">At</label>
                  <input id="sch-at" type="datetime-local" value={runAt} onChange={(e) => setRunAt(e.target.value)} className={inputSm} />
                </div>
              ) : (
                <>
                  <div>
                    <label htmlFor="sch-repeat" className="meta block">Repeat</label>
                    <select id="sch-repeat" value={repeat} onChange={(e) => setRepeat(e.target.value as Repeat)} className={inputSm}>
                      <option value="daily">Every day</option>
                      <option value="weekdays">Weekdays</option>
                      <option value="weekly">Every week</option>
                      <option value="custom">Cron expression</option>
                    </select>
                  </div>
                  {repeat === 'weekly' && (
                    <div>
                      <label htmlFor="sch-day" className="meta block">On</label>
                      <select id="sch-day" value={day} onChange={(e) => setDay(parseInt(e.target.value, 10))} className={inputSm}>
                        {DAYS.map((d, i) => <option key={d} value={i}>{d}</option>)}
                      </select>
                    </div>
                  )}
                  {repeat === 'custom' ? (
                    <div>
                      <label htmlFor="sch-cron" className="meta block">Cron (5 fields)</label>
                      <input id="sch-cron" value={custom} onChange={(e) => setCustom(e.target.value)} className={inputSm + ' font-mono'} />
                    </div>
                  ) : (
                    <div>
                      <label htmlFor="sch-time" className="meta block">At</label>
                      <input id="sch-time" type="time" value={time} onChange={(e) => setTime(e.target.value)} className={inputSm} />
                    </div>
                  )}
                </>
              )}
              <div>
                <label htmlFor="sch-tz" className="meta block">Time zone</label>
                <input id="sch-tz" value={tz} onChange={(e) => setTz(e.target.value)} className={inputSm} />
              </div>
            </div>
            <div className="flex flex-wrap gap-3 items-end">
              <div>
                <label htmlFor="sch-project" className="meta block">Project</label>
                <select id="sch-project" value={projectID} onChange={(e) => setProject(e.target.value)} className={inputSm}>
                  {chatProjects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                </select>
              </div>
              <div>
                <label htmlFor="sch-model" className="meta block">Model</label>
                <select id="sch-model" value={model} onChange={(e) => setModel(e.target.value)} className={inputSm}>
                  {selectors.map((m) => <option key={m} value={m}>{m}</option>)}
                </select>
              </div>
            </div>
            <label className="flex items-center gap-2"><input type="checkbox" checked={webFetch} onChange={(e) => setWebFetch(e.target.checked)} /> Let it read public web pages</label>
            <label className="flex items-center gap-2"><input type="checkbox" checked={reuse} onChange={(e) => setReuse(e.target.checked)} /> Keep adding to one conversation</label>
            <label className="flex items-center gap-2"><input type="checkbox" checked={email} onChange={(e) => setEmail(e.target.checked)} /> Email me when it finishes</label>
            <p className="meta">A repeat may run at most every 15 minutes. With no tools ticked the model only answers; it cannot run anything.</p>
            <button type="submit" className={btn.primarySm} disabled={!prompt.trim() || !projectID || create.isPending}>Schedule</button>
          </form>
        </section>
      </div>
    </div>
  )
}

function ScheduleRow({ s, onChange, onError }: { s: Schedule; onChange: () => void; onError: (e: unknown) => void }) {
  const [open, setOpen] = useState(false)
  const runs = useQuery({ queryKey: ['scheduled-runs', s.id, s.last_status], queryFn: () => api.get<{ runs: Run[] }>(`/api/scheduled/${s.id}/runs`), enabled: open })
  const toggle = useMutation({
    mutationFn: () =>
      api.put(`/api/scheduled/${s.id}`, {
        project_id: s.project_id, title: s.title, prompt: s.prompt, model: s.model, kind: s.kind, cron: s.cron, timezone: s.timezone,
        run_at: s.kind === 'once' ? (s.run_at ?? new Date(Date.now() + 3600_000).toISOString()) : null,
        enabled: !s.enabled, allow_web_fetch: s.allow_web_fetch, notify_email: s.notify_email, reuse_conversation: s.reuse_conversation,
      }),
    onSuccess: onChange,
    onError,
  })
  const runNow = useMutation({ mutationFn: () => api.post(`/api/scheduled/${s.id}/run`), onSuccess: onChange, onError })
  const del = useMutation({ mutationFn: () => api.del(`/api/scheduled/${s.id}`), onSuccess: onChange, onError })
  const conv = s.conversation_id

  return (
    <li className="px-3 py-2 space-y-1">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="truncate">{s.title}</p>
          <p className="meta">
            {when(s)} · {s.enabled ? (s.next_run_at ? `next ${new Date(s.next_run_at).toLocaleString()}` : 'finished') : 'paused'}
            {s.last_status ? ` · last: ${s.last_status}` : ''}
          </p>
          {s.last_error && <p className="text-danger text-xs">{s.last_error}</p>}
        </div>
        <div className="flex shrink-0 gap-3 text-xs">
          <button className="text-accent hover:text-accent-hover" onClick={() => runNow.mutate()} aria-label={`Run now: ${s.title}`}>Run now</button>
          <button className="text-accent hover:text-accent-hover" onClick={() => toggle.mutate()} aria-label={`${s.enabled ? 'Pause' : 'Resume'}: ${s.title}`}>{s.enabled ? 'Pause' : 'Resume'}</button>
          <button className={btn.danger} onClick={() => del.mutate()} aria-label={`Delete: ${s.title}`}>Delete</button>
        </div>
      </div>
      <div className="flex gap-3 text-xs">
        <button className="text-fg-2 hover:text-fg" onClick={() => setOpen(!open)} aria-expanded={open}>History</button>
        {conv && <Link to={`/c/${conv}`} className="text-accent border-b border-dotted border-link-underline hover:text-accent-hover">Open conversation</Link>}
      </div>
      {open && (
        <ul className="meta space-y-0.5 pt-1">
          {(runs.data?.runs ?? []).map((r) => (
            <li key={r.id}>
              {new Date(r.started_at).toLocaleString()} · {r.status}
              {r.error ? ` · ${r.error}` : ''}
              {r.conversation_id && (
                <>
                  {' · '}
                  <Link to={`/c/${r.conversation_id}`} className="text-accent border-b border-dotted border-link-underline">open</Link>
                </>
              )}
            </li>
          ))}
          {runs.data && runs.data.runs.length === 0 && <li>Has not run yet.</li>}
        </ul>
      )}
    </li>
  )
}
