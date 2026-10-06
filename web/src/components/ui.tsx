// Small structural pieces from the ws design system. Everything else is
// Tailwind utilities in place; see docs/DESIGN.md.
import clsx from 'clsx'

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
    <li className="flex items-center justify-between gap-3 px-3 py-2">
      <span className="min-w-0 truncate">{children}</span>
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
  danger: 'text-danger text-xs',
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
