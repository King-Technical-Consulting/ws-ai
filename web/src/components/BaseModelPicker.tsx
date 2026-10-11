import { useEffect, useId, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { api, type HubModel } from '../api'
import { inputSm } from './ui'

function paramsLabel(n?: number) {
  if (!n) return ''
  if (n >= 1e9) return `${(n / 1e9).toFixed(n >= 10e9 ? 0 : 1)}B`
  return `${Math.round(n / 1e6)}M`
}

function countLabel(n: number) {
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`
  if (n >= 1e3) return `${Math.round(n / 1e3)}k`
  return String(n)
}

/**
 * The base model field: a Hugging Face id, typed or picked. Typing searches
 * the hub through the server (debounced); an empty box lists the suggested
 * small bases. Arrow keys move, Enter picks, Escape closes; the typed text
 * is always accepted as is, so an id the hub does not list still works.
 */
export function BaseModelPicker({ value, onChange, label = 'Base model' }: { value: string; onChange: (id: string) => void; label?: string }) {
  const [open, setOpen] = useState(false)
  const [q, setQ] = useState(value)
  const [active, setActive] = useState(0)
  const [debounced, setDebounced] = useState('')
  const listId = useId()
  useEffect(() => {
    const t = setTimeout(() => setDebounced(q.trim()), 250)
    return () => clearTimeout(t)
  }, [q])
  const hits = useQuery({
    queryKey: ['training', 'hub', debounced],
    queryFn: () => api.get<{ models: HubModel[] }>(`/api/training/models?q=${encodeURIComponent(debounced)}`),
    enabled: open,
    staleTime: 10 * 60 * 1000,
    retry: false,
  })
  const models = hits.data?.models ?? []
  useEffect(() => setActive(0), [debounced, open])
  const pick = (id: string) => {
    onChange(id)
    setQ(id)
    setOpen(false)
  }
  return (
    <div className="relative flex-1 min-w-64">
      <input
        value={q}
        role="combobox"
        aria-expanded={open}
        aria-controls={listId}
        aria-autocomplete="list"
        aria-label={label}
        onChange={(e) => { setQ(e.target.value); onChange(e.target.value); setOpen(true) }}
        onFocus={() => setOpen(true)}
        onBlur={() => setTimeout(() => setOpen(false), 120)}
        onKeyDown={(e) => {
          if (!open && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) { setOpen(true); return }
          if (e.key === 'ArrowDown') { e.preventDefault(); setActive((a) => Math.min(a + 1, models.length - 1)) }
          else if (e.key === 'ArrowUp') { e.preventDefault(); setActive((a) => Math.max(a - 1, 0)) }
          else if (e.key === 'Enter' && open && models[active]) { e.preventDefault(); pick(models[active].id) }
          else if (e.key === 'Escape') setOpen(false)
        }}
        placeholder="base model: a Hugging Face id, e.g. Qwen/Qwen2.5-0.5B-Instruct"
        title="What the trainer downloads and trains on. Type to search the hub; required unless the endpoint's model name is already a Hugging Face id (org/name)"
        className={clsx(inputSm, 'font-mono')}
      />
      {open && (
        <ul id={listId} role="listbox" className="absolute z-20 mt-1 max-h-72 w-full overflow-y-auto rounded-lg border border-line bg-bg-2 py-1 font-sans text-sm">
          {hits.isError && <li className="px-3 py-1.5 meta">The model hub did not answer; type the id by hand.</li>}
          {!hits.isError && hits.isFetching && models.length === 0 && <li className="px-3 py-1.5 meta">Searching…</li>}
          {!hits.isError && !hits.isFetching && models.length === 0 && debounced && <li className="px-3 py-1.5 meta">No text-generation model matches; the id typed above is used as is.</li>}
          {!debounced && models.length > 0 && <li className="px-3 py-1 meta">suggested small bases</li>}
          {models.map((m, i) => (
            <li
              key={m.id}
              role="option"
              aria-selected={i === active}
              onMouseDown={(e) => { e.preventDefault(); pick(m.id) }}
              onMouseEnter={() => setActive(i)}
              className={clsx('flex cursor-pointer items-baseline gap-2 px-3 py-1.5', i === active ? 'bg-bg-3' : 'hover:bg-bg-3')}
            >
              <span className="font-mono text-xs truncate">{m.id}</span>
              <span className="meta ml-auto shrink-0 tnum">
                {paramsLabel(m.params)}
                {m.downloads ? ` · ${countLabel(m.downloads)} downloads` : ''}
                {m.gated ? ' · gated' : ''}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
