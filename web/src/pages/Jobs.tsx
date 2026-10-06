import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import clsx from 'clsx'
import { api, type CCJob, type CCJobsResponse } from '../api'
import { SectionHead, PageTitle, Callout, btn } from '../components/ui'

/**
 * Read-only Claude Code job tab (docs/CLAUDE_CODE_JOBS.md §6.5). The list
 * is cc_jobs (handles reported by wsj and the router, liveness refreshed
 * from each target's tmux); the terminal is `tmux attach -r` on the job's
 * window relayed over a WebSocket. Nothing typed here reaches the job, and
 * nothing shown is kept.
 */
export default function Jobs() {
  const qc = useQueryClient()
  const [selected, setSelected] = useState<string | null>(null)
  const jobs = useQuery({
    queryKey: ['ccjobs'],
    queryFn: () => api.get<CCJobsResponse>('/api/jobs/cc'),
    refetchInterval: 10_000,
    retry: false,
  })
  const [forcing, setForcing] = useState(false)
  const refreshNow = async () => {
    setForcing(true)
    try {
      const r = await api.get<CCJobsResponse>('/api/jobs/cc?refresh=force')
      qc.setQueryData(['ccjobs'], r)
    } catch {
      /* the query's own error shows */
    } finally {
      setForcing(false)
    }
  }
  const list = jobs.data?.jobs ?? []
  const current = list.find((j) => j.id === selected) ?? null

  return (
    <div className="flex-1 min-h-0 flex">
      <div className="w-[26rem] shrink-0 border-r border-line overflow-y-auto">
        <div className="p-6 space-y-6">
          <PageTitle>Claude Code jobs</PageTitle>
          <p className="reading text-sm text-fg-2">
            Sessions started with <code className="font-mono text-xs">wsj run</code> or by the router. The list is what each target's tmux
            reports; the terminal is a read-only view of the real window.
          </p>
          {jobs.error && <Callout kind="error">{(jobs.error as Error).message}</Callout>}
          {jobs.data?.budget && <BudgetNote b={jobs.data.budget} />}
          <section className="space-y-3">
            <SectionHead aside={jobs.data ? <RefreshNote r={jobs.data.refresh} /> : undefined}>jobs</SectionHead>
            <ul className="divide-y divide-line rounded-lg border border-line text-sm">
              {list.map((j) => (
                <li key={j.id}>
                  <button
                    onClick={() => setSelected(j.id)}
                    className={clsx('w-full text-left px-3 py-2 space-y-0.5', selected === j.id ? 'bg-bg-3' : 'hover:bg-bg-2')}
                  >
                    <div className="flex items-center justify-between gap-3">
                      <span className="font-mono text-xs truncate">
                        {j.id} <span className="text-fg-3">on {j.target}</span>
                      </span>
                      <State s={j.status} />
                    </div>
                    <div className="meta truncate">
                      {ago(j.started_at)}
                      {j.model ? ` · ${j.model}` : ''}
                      {j.cwd ? ` · ${j.cwd}` : ''}
                    </div>
                  </button>
                </li>
              ))}
              {jobs.isSuccess && list.length === 0 && <li className="px-3 py-3 meta">No jobs yet. Launch one with wsj run.</li>}
            </ul>
            <div className="flex items-center gap-3">
              <button onClick={() => void refreshNow()} disabled={forcing} className={btn.secondarySm}>
                {forcing ? 'Refreshing…' : 'Refresh now'}
              </button>
              {jobs.data?.refresh.targets.filter((t) => !t.ok).map((t) => (
                <span key={t.name} className="meta truncate" title={t.error}>
                  {t.name}: {t.error}
                </span>
              ))}
            </div>
          </section>
        </div>
      </div>
      <div className="flex-1 min-w-0 min-h-0 flex flex-col">
        {current ? <JobTerm key={current.id} job={current} /> : <p className="meta p-6">Pick a job to watch its terminal.</p>}
      </div>
    </div>
  )
}

function RefreshNote({ r }: { r: CCJobsResponse['refresh'] }) {
  if (r.error) return <>{r.error}</>
  if (!r.at || r.at.startsWith('0001')) return <>not refreshed yet</>
  const ok = r.targets.filter((t) => t.ok).length
  return (
    <>
      refreshed {ago(r.at)} · {ok}/{r.targets.length} targets
    </>
  )
}

// BudgetNote is the week's launch count against the soft cap. Launches
// are counted locally (every job ws knows of); Claude's own usage is
// never read, so this is a proxy for the subscription's weekly pool.
function BudgetNote({ b }: { b: NonNullable<CCJobsResponse['budget']> }) {
  const week = new Date(b.week).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  if (b.cap <= 0) {
    return (
      <p className="meta">
        {b.used} launch{b.used === 1 ? '' : 'es'} since {week}. No weekly cap is set (WS_CC_WEEKLY_CAP).
      </p>
    )
  }
  const pct = Math.min(100, Math.round((b.used / b.cap) * 100))
  const label = b.level === 'full' ? 'cap reached: the router sends everything to the API lane unless a lane is asked for' : b.level === 'soft' ? 'soft threshold reached: only repo-bound work goes to the subscription' : 'under the soft threshold'
  return (
    <div className="space-y-1.5">
      <p className="meta">
        {b.used} of {b.cap} launches since {week} ({pct}%). {label}.
      </p>
      <div className="h-px w-full bg-line relative" aria-hidden>
        <div className="absolute inset-y-0 left-0 bg-fg-3" style={{ width: `${pct}%`, height: '1px' }} />
      </div>
    </div>
  )
}

function State({ s }: { s: CCJob['status'] }) {
  const label = s === 'alive' ? 'running' : s === 'dead' ? 'exited' : s
  return <span className={clsx('section-head border-0 shrink-0', s === 'alive' ? 'text-fg' : 'text-fg-3')}>{label}</span>
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

// ---- terminal ----

function JobTerm({ job }: { job: CCJob }) {
  const host = useRef<HTMLDivElement>(null)
  const [state, setState] = useState<'connecting' | 'open' | 'closed'>('connecting')
  const [reason, setReason] = useState<string>('')
  const [gen, setGen] = useState(0)
  useEffect(() => {
    if (!host.current) return
    const cs = getComputedStyle(document.documentElement)
    const term = new Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      cursorBlink: false,
      disableStdin: true,
      theme: {
        background: cs.getPropertyValue('--color-bg').trim() || '#1b1611',
        foreground: cs.getPropertyValue('--color-fg').trim() || '#e8e2d8',
        cursor: cs.getPropertyValue('--color-fg').trim() || '#e8e2d8',
        selectionBackground: cs.getPropertyValue('--color-bg-3').trim() || '#2e261f',
      },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(host.current)
    fit.fit()
    setReason('')
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const ws = new WebSocket(`${proto}://${location.host}/api/jobs/cc/${job.id}/term?cols=${term.cols}&rows=${term.rows}`)
    ws.binaryType = 'arraybuffer'
    ws.onopen = () => setState('open')
    ws.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(ev.data))
        return
      }
      try {
        const m = JSON.parse(String(ev.data)) as { type?: string; message?: string }
        if (m.type === 'error' && m.message) setReason(m.message)
      } catch {
        /* not ours */
      }
    }
    ws.onclose = () => {
      setState('closed')
      term.write('\r\n\x1b[2m[view closed]\x1b[0m\r\n')
    }
    ws.onerror = () => setState('closed')
    // Size is the only thing sent. Keystrokes are not forwarded (and the
    // attach is read-only on the tmux side as well).
    const onResize = term.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'resize', cols, rows }))
    })
    const ro = new ResizeObserver(() => {
      try {
        fit.fit()
      } catch {
        /* unmounted */
      }
    })
    ro.observe(host.current)
    return () => {
      ro.disconnect()
      onResize.dispose()
      ws.close()
      term.dispose()
    }
  }, [job.id, gen])
  return (
    <>
      <div className="h-12 shrink-0 border-b border-line flex items-center justify-between px-4 gap-3">
        <span className="meta truncate">
          <span className="font-mono text-xs text-fg-2">{job.id}</span> on {job.target} · {job.session}:{job.window} · read-only · {state}
        </span>
        <div className="flex items-center gap-3 shrink-0">
          <span className="meta">attach: wsj attach --on {job.target} {job.id}</span>
          {state === 'closed' && (
            <button
              onClick={() => {
                setState('connecting')
                setGen((g) => g + 1)
              }}
              className={clsx(btn.secondarySm, 'py-0.5 px-2 text-xs')}
            >
              Reconnect
            </button>
          )}
        </div>
      </div>
      {reason && (
        <div className="px-4 pt-3">
          <Callout kind="error">{reason}</Callout>
        </div>
      )}
      <div ref={host} className="flex-1 min-h-0 p-1" />
    </>
  )
}
