// Admin, Models: finding one endpoint or provider among many. The search text, the filters and the
// provider chosen on the Providers list live in the URL hash (#models?q=qwen&tier=small), the way the
// open group does, so a reload or a link lands on the same view. The pure parts (matching, grouping)
// are functions of the rows so they can be read on their own.
import { useEffect, useState } from 'react'
import clsx from 'clsx'
import { inputSm } from '../components/ui'

export type ModelFilters = {
  q: string
  provider: string
  enabled: '' | 'on' | 'off'
  routable: '' | 'yes' | 'no'
  where: '' | 'local' | 'hosted'
  health: '' | 'healthy' | 'degraded' | 'down'
  tier: '' | 'small' | 'medium' | 'large'
  /** The search box over the providers list. */
  pq: string
}

const EMPTY: ModelFilters = { q: '', provider: '', enabled: '', routable: '', where: '', health: '', tier: '', pq: '' }
const KEYS = Object.keys(EMPTY) as (keyof ModelFilters)[]

function readHash(): ModelFilters {
  const raw = window.location.hash.replace(/^#/, '')
  const params = new URLSearchParams(raw.includes('?') ? raw.slice(raw.indexOf('?') + 1) : '')
  const f = { ...EMPTY }
  for (const k of KEYS) (f as Record<string, string>)[k] = params.get(k) ?? ''
  return f
}

function writeHash(f: ModelFilters) {
  const params = new URLSearchParams()
  for (const k of KEYS) if (f[k]) params.set(k, f[k])
  const qs = params.toString()
  window.history.replaceState(null, '', `#models${qs ? `?${qs}` : ''}`)
}

/** The Models group's search and filters, kept in the hash. */
export function useModelFilters(): [ModelFilters, (patch: Partial<ModelFilters>) => void, () => void] {
  const [f, setF] = useState<ModelFilters>(readHash)
  const set = (patch: Partial<ModelFilters>) => {
    const next = { ...f, ...patch }
    setF(next)
    writeHash(next)
  }
  const clear = () => {
    setF(EMPTY)
    writeHash(EMPTY)
  }
  return [f, set, clear]
}

export type FindableEndpoint = {
  id: string
  display_name: string
  provider_id: string
  model_name: string
  enabled: boolean
  local: boolean
  tier?: string
  not_routable?: boolean
  health: { status: string }
}

/** True when the endpoint passes the search and every filter that is set (they combine). */
export function endpointMatches(e: FindableEndpoint, f: ModelFilters): boolean {
  if (f.provider && e.provider_id !== f.provider) return false
  if (f.enabled && (f.enabled === 'on') !== e.enabled) return false
  if (f.routable && (f.routable === 'yes') !== !e.not_routable) return false
  if (f.where && (f.where === 'local') !== e.local) return false
  if (f.health && e.health.status !== f.health) return false
  if (f.tier && (e.tier ?? '') !== f.tier) return false
  const words = f.q.toLowerCase().split(/\s+/).filter(Boolean)
  if (words.length) {
    const hay = [e.id, e.display_name, e.provider_id, e.model_name, e.tier ?? ''].join(' ').toLowerCase()
    if (!words.every((w) => hay.includes(w))) return false
  }
  return true
}

export function filtersActive(f: ModelFilters): boolean {
  return !!(f.q || f.provider || f.enabled || f.routable || f.where || f.health || f.tier)
}

export type EndpointGroup<E extends FindableEndpoint> = {
  provider: string
  total: number
  rows: E[] // the matching rows, enabled first, then by name
  health: { healthy: number; degraded: number; down: number }
}

/** Groups endpoints by provider (providers by id), keeping a group only if a row matches, or if nothing is being filtered. */
export function groupEndpoints<E extends FindableEndpoint>(all: E[], f: ModelFilters): EndpointGroup<E>[] {
  const by = new Map<string, E[]>()
  for (const e of all) by.set(e.provider_id, [...(by.get(e.provider_id) ?? []), e])
  const groups: EndpointGroup<E>[] = []
  for (const provider of [...by.keys()].sort()) {
    const members = by.get(provider)!
    const rows = members
      .filter((e) => endpointMatches(e, f))
      .sort((a, b) => Number(b.enabled) - Number(a.enabled) || (a.display_name || a.id).localeCompare(b.display_name || b.id))
    if (rows.length === 0 && filtersActive(f)) continue
    const health = { healthy: 0, degraded: 0, down: 0 }
    for (const e of members) if (e.health.status in health) health[e.health.status as keyof typeof health]++
    groups.push({ provider, total: members.length, rows, health })
  }
  return groups
}

/** "2 healthy · 1 down", leaving out what is zero. */
export function healthSummary(h: { healthy: number; degraded: number; down: number }): string {
  return [h.healthy && `${h.healthy} healthy`, h.degraded && `${h.degraded} degraded`, h.down && `${h.down} down`].filter(Boolean).join(' · ')
}

/** A small ring for an endpoint's health. Outlined, never filled: the olive and the red are the design's, and the word is always beside it. */
export function HealthDot({ status }: { status: string }) {
  const tone = status === 'healthy' ? 'border-ok' : status === 'down' ? 'border-danger bg-danger' : status === 'degraded' ? 'border-ok border-dashed' : 'border-fg-4'
  return <span aria-hidden="true" title={status} className={clsx('inline-block h-2 w-2 shrink-0 rounded-full border', tone)} />
}

const tog = (on: boolean) =>
  clsx('min-h-9 md:min-h-7 rounded-md border px-2 text-xs inline-flex items-center', on ? 'border-line bg-bg-3 text-fg' : 'border-line-soft text-fg-2 hover:bg-bg-2')

function Toggle<T extends string>({ label, value, options, onChange }: { label: string; value: T | ''; options: readonly { value: T; text: string }[]; onChange: (v: T | '') => void }) {
  return (
    <div role="group" aria-label={label} className="flex items-center gap-1">
      {options.map((o) => (
        <button key={o.value} type="button" aria-pressed={value === o.value} onClick={() => onChange(value === o.value ? '' : o.value)} className={tog(value === o.value)}>
          {o.text}
        </button>
      ))}
    </div>
  )
}

/** The endpoint search box (press / to reach it), the filters and the count. */
export function EndpointFilterBar({ f, set, clear, shown, total, inputRef }: { f: ModelFilters; set: (p: Partial<ModelFilters>) => void; clear: () => void; shown: number; total: number; inputRef: React.RefObject<HTMLInputElement | null> }) {
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <input
          ref={inputRef}
          type="search"
          value={f.q}
          onChange={(e) => set({ q: e.target.value })}
          placeholder="Search models: id, name, provider, tier (press /)"
          aria-label="Search models"
          className={clsx(inputSm, 'min-h-9 md:min-h-0')}
        />
        <span className="meta shrink-0 tnum" aria-live="polite">{shown} of {total}</span>
      </div>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Toggle label="Enabled" value={f.enabled} onChange={(v) => set({ enabled: v })} options={[{ value: 'on', text: 'enabled' }, { value: 'off', text: 'disabled' }]} />
        <Toggle label="Routing" value={f.routable} onChange={(v) => set({ routable: v })} options={[{ value: 'yes', text: 'routable' }, { value: 'no', text: 'name only' }]} />
        <Toggle label="Where it runs" value={f.where} onChange={(v) => set({ where: v })} options={[{ value: 'local', text: 'local' }, { value: 'hosted', text: 'hosted' }]} />
        <Toggle label="Health" value={f.health} onChange={(v) => set({ health: v })} options={[{ value: 'healthy', text: 'healthy' }, { value: 'degraded', text: 'degraded' }, { value: 'down', text: 'down' }]} />
        <Toggle label="Tier" value={f.tier} onChange={(v) => set({ tier: v })} options={[{ value: 'small', text: 'small' }, { value: 'medium', text: 'medium' }, { value: 'large', text: 'large' }]} />
        {f.provider && (
          <button type="button" onClick={() => set({ provider: '' })} className={tog(true)} aria-label={`Clear the provider filter, ${f.provider}`}>
            provider: {f.provider} ×
          </button>
        )}
        {filtersActive(f) && (
          <button type="button" onClick={clear} className="min-h-9 md:min-h-7 px-2 text-xs text-fg-2 hover:text-fg underline decoration-dotted">
            clear filters
          </button>
        )}
      </div>
    </div>
  )
}

/** Press / anywhere on the page (outside a field) to focus the box. */
export function useSlashFocus(ref: React.RefObject<HTMLInputElement | null>) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey) return
      const t = e.target as HTMLElement | null
      if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable)) return
      e.preventDefault()
      ref.current?.focus()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [ref])
}
