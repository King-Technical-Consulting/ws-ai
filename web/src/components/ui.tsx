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
