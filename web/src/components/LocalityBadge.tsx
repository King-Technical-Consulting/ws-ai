import { useEffect, useRef, useState } from 'react'
import clsx from 'clsx'
import type { Locality } from '../api'

/** What a request from here needs for a sensitive action, in words: the part of the note about step-up. */
export function stepUpNote(info: Locality, until?: string | null, hasPasskey?: boolean, now = Date.now()): string {
  if (!info.configured || info.locality !== 'away') return ''
  if (!hasPasskey) return 'no passkey on this account: sensitive actions are refused from here'
  const left = until ? Math.ceil((Date.parse(until) - now) / 60000) : 0
  if (left > 0) return `unlocked for ${left} more ${left === 1 ? 'minute' : 'minutes'}`
  return 'a passkey tap is needed for sensitive actions'
}

/**
 * Whether ws counts this request as home or away, as a small-caps mark. Hidden while WS_HOME_NETWORKS
 * is unset (nothing is judged then). A tap shows how ws decided; the title says the same.
 */
export function LocalityBadge({ info, stepUp, className }: { info?: Locality; stepUp?: { until?: string | null; hasPasskey?: boolean }; className?: string }) {
  const [open, setOpen] = useState(false)
  const box = useRef<HTMLSpanElement>(null)
  useEffect(() => {
    if (!open) return
    const away = (e: MouseEvent) => { if (!box.current?.contains(e.target as Node)) setOpen(false) }
    document.addEventListener('mousedown', away)
    return () => document.removeEventListener('mousedown', away)
  }, [open])
  if (!info?.configured) return null
  const away = info.locality === 'away'
  const step = stepUpNote(info, stepUp?.until, stepUp?.hasPasskey)
  const note = `${info.reason}${info.address ? ` (${info.address})` : ''}${step ? `; ${step}` : ''}`
  return (
    <span ref={box} className={clsx('relative inline-flex', className)} onKeyDown={(e) => { if (e.key === 'Escape') setOpen(false) }}>
      <button
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-label={`Where ws sees you: ${info.locality}`}
        title={note}
        className={clsx('section-head border-0 inline-flex items-center justify-center rounded-md min-h-9 min-w-9 px-2 hover:bg-bg-3', away ? 'text-accent' : 'text-fg-3')}
      >
        {info.locality}
      </button>
      {open && (
        <span role="status" className="absolute bottom-full left-0 mb-1 z-50 w-56 rounded-lg border border-line bg-bg p-2 text-xs normal-case tracking-normal text-fg-2 md:bottom-auto md:top-full md:mt-1 md:mb-0">
          {note}
        </span>
      )}
    </span>
  )
}
