import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { X } from 'lucide-react'
import { api, type ConversationSettings, type DesignContext } from '../api'
import { SectionHead, Callout, btn, inputSm } from './ui'

const COLOR_TOKENS = ['primary', 'background', 'foreground', 'accent', 'muted', 'border']
const TYPE_TOKENS = ['heading', 'body', 'mono']

/**
 * The design system a design conversation's mockups follow. Saved into
 * the conversation's settings.design_context; the server puts it in the
 * system prompt and the model copies it onto each design artifact.
 */
export function DesignPanel({ conversationId, settings, onClose }: { conversationId: string; settings: ConversationSettings | null | undefined; onClose: () => void }) {
  const qc = useQueryClient()
  const cur = settings?.design_context ?? {}
  const [library, setLibrary] = useState<DesignContext['library'] | ''>(cur.library ?? '')
  const [colors, setColors] = useState<Record<string, string>>({ ...cur.colors })
  const [type, setType] = useState<Record<string, string>>({ ...cur.type })
  const [spacing, setSpacing] = useState(cur.spacing ?? '')
  const [radius, setRadius] = useState(cur.radius ?? '')
  const [components, setComponents] = useState((cur.components ?? []).join(', '))
  const [notes, setNotes] = useState(cur.notes ?? '')
  const [saved, setSaved] = useState(false)

  const save = useMutation({
    mutationFn: () => {
      const clean = (m: Record<string, string>) => Object.fromEntries(Object.entries(m).filter(([, v]) => v.trim() !== '').map(([k, v]) => [k, v.trim()]))
      const design_context: DesignContext = {}
      if (library) design_context.library = library
      const c = clean(colors)
      if (Object.keys(c).length) design_context.colors = c
      const t = clean(type)
      if (Object.keys(t).length) design_context.type = t
      if (spacing.trim()) design_context.spacing = spacing.trim()
      if (radius.trim()) design_context.radius = radius.trim()
      const comps = components.split(',').map((s) => s.trim()).filter(Boolean)
      if (comps.length) design_context.components = comps
      if (notes.trim()) design_context.notes = notes.trim()
      const next: ConversationSettings = { ...(settings ?? {}), design_context: Object.keys(design_context).length ? design_context : undefined }
      return api.patch(`/api/conversations/${conversationId}`, { settings: next })
    },
    onSuccess: () => {
      setSaved(true)
      qc.invalidateQueries({ queryKey: ['conv', conversationId] })
    },
  })

  return (
    <aside className="fixed inset-0 z-20 md:static md:z-auto md:w-[46%] md:min-w-[380px] md:shrink-0 border-l border-line bg-bg-2 flex flex-col">
      <header className="h-12 shrink-0 border-b border-line flex items-center gap-2 px-3">
        <span className="min-w-0 flex-1 truncate reading-tight">Design system</span>
        <button onClick={onClose} className="p-1.5 rounded-md text-fg-2 hover:bg-bg-3 hover:text-fg" title="Close" aria-label="Close">
          <X size={16} />
        </button>
      </header>
      <div className="flex-1 min-h-0 overflow-y-auto p-4 space-y-5 text-sm">
        <p className="text-fg-2">
          What every mockup in this conversation follows. The assistant puts these tokens on each design it makes, so versions and variants stay consistent. Leave a field empty to let the model choose.
        </p>
        <section className="space-y-2">
          <SectionHead>library</SectionHead>
          <select value={library} onChange={(e) => setLibrary(e.target.value as DesignContext['library'] | '')} className={inputSm} aria-label="Component library">
            <option value="">model's choice</option>
            <option value="tailwind">Tailwind CSS (CDN)</option>
            <option value="shadcn">Tailwind with shadcn/ui conventions</option>
            <option value="plain">plain CSS</option>
          </select>
        </section>
        <section className="space-y-2">
          <SectionHead>colors</SectionHead>
          <div className="grid grid-cols-2 gap-2">
            {COLOR_TOKENS.map((k) => (
              <label key={k} className="flex items-center gap-2">
                <span className="w-24 shrink-0 text-xs text-fg-2">{k}</span>
                <span className="h-5 w-5 shrink-0 rounded-md border border-line" style={{ background: colors[k] || 'transparent' }} aria-hidden />
                <input value={colors[k] ?? ''} onChange={(e) => setColors({ ...colors, [k]: e.target.value })} placeholder="#rrggbb" className={inputSm} aria-label={`${k} color`} />
              </label>
            ))}
          </div>
        </section>
        <section className="space-y-2">
          <SectionHead>type</SectionHead>
          {TYPE_TOKENS.map((k) => (
            <label key={k} className="flex items-center gap-2">
              <span className="w-24 shrink-0 text-xs text-fg-2">{k}</span>
              <input value={type[k] ?? ''} onChange={(e) => setType({ ...type, [k]: e.target.value })} placeholder={k === 'mono' ? 'ui-monospace, Menlo' : 'Inter, system-ui'} className={inputSm} aria-label={`${k} font`} />
            </label>
          ))}
        </section>
        <section className="space-y-2">
          <SectionHead>spacing and radius</SectionHead>
          <div className="grid grid-cols-2 gap-2">
            <input value={spacing} onChange={(e) => setSpacing(e.target.value)} placeholder="spacing unit, e.g. 8px" className={inputSm} aria-label="Spacing unit" />
            <input value={radius} onChange={(e) => setRadius(e.target.value)} placeholder="radius, e.g. 8px" className={inputSm} aria-label="Corner radius" />
          </div>
        </section>
        <section className="space-y-2">
          <SectionHead>components</SectionHead>
          <input value={components} onChange={(e) => setComponents(e.target.value)} placeholder="button, card, input, nav, table (comma separated)" className={inputSm} aria-label="Component catalog" />
        </section>
        <section className="space-y-2">
          <SectionHead>notes</SectionHead>
          <textarea value={notes} onChange={(e) => setNotes(e.target.value)} rows={4} placeholder="Voice, what to avoid, accessibility rules, the product this is for." className={inputSm} aria-label="Notes" />
        </section>
        <div className="flex items-center gap-3">
          <button onClick={() => save.mutate()} disabled={save.isPending} className={btn.primarySm}>
            Save
          </button>
          {saved && !save.isPending && <span className="meta">Saved. It applies from the next message.</span>}
        </div>
        {save.error && <Callout kind="error">{save.error.message}</Callout>}
      </div>
    </aside>
  )
}
