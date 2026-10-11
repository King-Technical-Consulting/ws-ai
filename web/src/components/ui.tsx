// Small structural pieces from the ws design system. Everything else is
// Tailwind utilities in place; see docs/DESIGN.md.
import clsx from 'clsx'
import { Star } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'

export { DialogProvider, useConfirm, usePrompt } from './dialogs'
export type { ConfirmOptions, PromptOptions } from './dialogs'

/** Small-caps section head with a quiet rule under it. Text is written lowercase. */
export function SectionHead({ children, aside, className }: { children: React.ReactNode; aside?: React.ReactNode; className?: string }) {
  return (
    <h2 className={clsx('section-head flex items-baseline justify-between gap-3', className)}>
      <span>{children}</span>
      {aside && <small className="meta normal-case tracking-normal text-fg-4">{aside}</small>}
    </h2>
  )
}

/** Serif page title, regular weight. */
export function PageTitle({ children, className }: { children: React.ReactNode; className?: string }) {
  return <h1 className={clsx('page-title', className)}>{children}</h1>
}

/** Hairline divider with an optional italic word in the middle. */
export function Divider({ children }: { children?: React.ReactNode }) {
  return (
    <div className="flex items-center gap-3 meta">
      <div className="h-px flex-1 bg-line" />
      {children}
      {children && <div className="h-px flex-1 bg-line" />}
    </div>
  )
}

/** Note or error callout. Tints are opacity on a token. */
export function Callout({ kind = 'note', children }: { kind?: 'note' | 'error'; children: React.ReactNode }) {
  return (
    <div
      className={clsx(
        'text-sm p-3',
        kind === 'note' && 'rounded-lg border border-accent/50 bg-bg-2',
        kind === 'error' && 'rounded-md border border-danger/40 text-danger',
      )}
    >
      {children}
    </div>
  )
}

/** Bordered list with hairline dividers between rows. */
export function ListGroup({ children, empty }: { children: React.ReactNode; empty?: string }) {
  const hasRows = Array.isArray(children) ? children.some(Boolean) : Boolean(children)
  return (
    <ul className="divide-y divide-line rounded-lg border border-line text-sm">
      {hasRows ? children : <li className="px-3 py-3 meta">{empty ?? 'Nothing here yet.'}</li>}
    </ul>
  )
}

export function Row({ children, action }: { children: React.ReactNode; action?: React.ReactNode }) {
  return (
    // The text wraps, and breaks inside a long unbroken name, instead of being cut to make room;
    // when the line is too narrow for both, the actions drop below it.
    <li className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 px-3 py-2">
      <span className="min-w-0 flex-1 basis-56 [overflow-wrap:anywhere]">{children}</span>
      {action}
    </li>
  )
}

const base = 'inline-flex items-center justify-center gap-2 shrink-0 whitespace-nowrap rounded-lg disabled:opacity-60'
export const btn = {
  primary: `${base} bg-accent text-accent-fg px-4 py-2.5 font-medium hover:bg-accent-strong`,
  secondary: `${base} border border-line px-4 py-2.5 font-medium hover:bg-bg-2`,
  primarySm: `${base} bg-accent text-accent-fg px-4 py-2 text-sm font-medium hover:bg-accent-strong`,
  secondarySm: `${base} border border-line px-4 py-2 text-sm hover:bg-bg-2`,
  // Small text actions in a row (remove, download, evaluate). A phone gets a finger-sized box; the text stays small.
  link: 'text-xs text-fg-2 hover:text-fg inline-flex items-center justify-center min-h-9 min-w-9 md:min-h-0 md:min-w-0',
  danger: 'text-danger text-xs inline-flex items-center justify-center min-h-9 min-w-9 md:min-h-0 md:min-w-0',
}

export const input = 'w-full rounded-lg border border-line bg-bg-2 px-3 py-2.5 outline-none focus:border-accent'
export const inputSm = 'w-full rounded-lg border border-line bg-bg-2 px-3 py-2 text-sm outline-none focus:border-accent'

/** The routing mark: one line splits to a hosted model (filled dot) and a local one (ring).
 *  Ink follows currentColor, the destinations use the accent. `draw` plays the draw-on once (Login only). */
export function WsMark({ size = 24, draw = false, className }: { size?: number; draw?: boolean; className?: string }) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 64 64"
      width={size}
      height={size}
      fill="none"
      role="img"
      aria-label="ws"
      className={className}
    >
      {/* thicker strokes below 32px so the mark holds at small sizes */}
      <g stroke="currentColor" strokeWidth={size < 32 ? 4 : 2.6} strokeLinecap="round">
        <path className={draw ? 'ws-draw' : undefined} d="M6 32 H22 C34 32 36 14 50 14" />
        <path className={draw ? 'ws-draw-2' : undefined} d="M22 32 C34 32 36 50 50 50" />
      </g>
      <circle cx="5" cy="32" r="2.6" fill="currentColor" />
      <circle className={draw ? 'ws-dest-1' : undefined} cx="55" cy="14" r="4.5" fill="var(--color-accent)" />
      <circle className={draw ? 'ws-dest-2' : undefined} cx="55" cy="50" r="3.6" stroke="var(--color-accent)" strokeWidth="2" />
    </svg>
  )
}

/** Outlined status chip. `ok` is healthy or local, `info` is hosted, queued or informational.
 *  Never interactive: orange stays the only interactive hue. */
export function StatusChip({ kind, children }: { kind: 'ok' | 'info'; children: React.ReactNode }) {
  return (
    <span
      className={clsx(
        'inline-flex items-center rounded-md border px-2.5 py-1 text-xs',
        kind === 'ok' && 'border-ok text-ok',
        kind === 'info' && 'border-info text-info',
      )}
    >
      {children}
    </span>
  )
}

/** The open group of a page with a section nav, kept in the URL hash so a link or a reload lands on it.
 *  An unknown or missing hash is the first id. */
export function useSectionHash<T extends string>(ids: readonly T[]): [T, (id: T) => void] {
  const read = () => {
    // A hash may carry a page's own view after a ?, as in #models?q=qwen; only the part before it names the group.
    const h = window.location.hash.replace(/^#/, '').split('?')[0]
    return (ids as readonly string[]).includes(h) ? (h as T) : ids[0]
  }
  const [current, setCurrent] = useState<T>(read)
  useEffect(() => {
    const onHash = () => setCurrent(read())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  const choose = (id: T) => {
    window.history.replaceState(null, '', `#${id}`)
    setCurrent(id)
  }
  return [current, choose]
}

/** True from the md breakpoint (768px) up. Used where two layouts must not both be in the page. */
function useWide(): boolean {
  const q = '(min-width: 768px)'
  const [wide, setWide] = useState(() => typeof window.matchMedia === 'function' && window.matchMedia(q).matches)
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return
    const m = window.matchMedia(q)
    const on = () => setWide(m.matches)
    on()
    m.addEventListener('change', on)
    return () => m.removeEventListener('change', on)
  }, [])
  return wide
}

/** Section nav for a long page: a list down the left on wide screens, a select on a phone.
 *  Pairs with useSectionHash; render only the open group beside it. */
export function SectionNav<T extends string>({ items, value, onChange, label }: { items: readonly { id: T; label: string }[]; value: T; onChange: (id: T) => void; label: string }) {
  const wide = useWide()
  return (
    <nav aria-label={label} className="md:w-40 md:shrink-0">
      {wide ? (
        <ul className="space-y-1 sticky top-6">
          {items.map((i) => (
            <li key={i.id}>
              <button
                type="button"
                onClick={() => onChange(i.id)}
                aria-current={i.id === value ? 'page' : undefined}
                className={clsx('w-full text-left rounded-md px-3 py-1.5 text-sm', i.id === value ? 'bg-bg-2 text-fg' : 'text-fg-2 hover:bg-bg-2')}
              >
                {i.label}
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <select value={value} onChange={(e) => onChange(e.target.value as T)} aria-label={label} className={inputSm}>
          {items.map((i) => <option key={i.id} value={i.id}>{i.label}</option>)}
        </select>
      )}
    </nav>
  )
}

/** A quiet "More" menu for a row's rarely used actions. Children are buttons; picking one closes it. */
export function RowMenu({ label, children }: { label: string; children: React.ReactNode }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const away = (e: MouseEvent) => { if (!ref.current?.contains(e.target as Node)) setOpen(false) }
    const esc = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', away)
    document.addEventListener('keydown', esc)
    return () => { document.removeEventListener('mousedown', away); document.removeEventListener('keydown', esc) }
  }, [open])
  return (
    <div ref={ref} className="relative">
      <button type="button" onClick={() => setOpen((o) => !o)} aria-haspopup="menu" aria-expanded={open} aria-label={label} className={btn.secondarySm}>
        More
      </button>
      {open && (
        <div role="menu" onClick={() => setOpen(false)} className="absolute right-0 z-10 mt-1 flex min-w-44 flex-col items-stretch gap-1 rounded-lg border border-line bg-bg p-2 [&>button]:text-left [&>button]:px-2 [&>button]:py-1.5">
          {children}
        </div>
      )}
    </div>
  )
}

/** A star toggle for favorites: filled in the accent when starred. */
export function StarButton({ starred, onToggle, label = 'Favorite', className }: { starred: boolean; onToggle: () => void; label?: string; className?: string }) {
  return (
    <button
      onClick={onToggle}
      className={clsx('p-1 rounded-md hover:bg-bg-3', starred ? 'text-accent' : 'text-fg-4 hover:text-fg', className)}
      title={starred ? 'Remove from favorites' : 'Add to favorites'}
      aria-label={label}
      aria-pressed={starred}
    >
      <Star size={13} fill={starred ? 'currentColor' : 'none'} />
    </button>
  )
}
