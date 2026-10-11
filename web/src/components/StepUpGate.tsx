import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { setStepUpHandler } from '../api'
import { stepUp } from '../lib/passkey'
import { btn, Callout } from './ui'

/**
 * The step-up dialog (PLAN M5). A sensitive action from away is answered 403 step_up_required until a
 * passkey assertion has been made on this session; the API client waits for this dialog, then sends the
 * request again. One dialog serves every request that is waiting. It asks with a button, not on its own:
 * browsers want a tap before they show a passkey prompt. Cancel or Escape lets the original error through.
 */
export function StepUpGate() {
  const qc = useQueryClient()
  const dialog = useRef<HTMLDialogElement>(null)
  const waiting = useRef<Promise<boolean> | null>(null)
  const settle = useRef<((ok: boolean) => void) | null>(null)
  const opener = useRef<HTMLElement | null>(null)
  const [state, setState] = useState<{ working: boolean; error: string | null } | null>(null)

  useEffect(() => {
    setStepUpHandler(() => {
      if (waiting.current) return waiting.current // several requests, one dialog
      opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
      waiting.current = new Promise<boolean>((resolve) => { settle.current = resolve })
      setState({ working: false, error: null })
      return waiting.current
    })
    return () => setStepUpHandler(null)
  }, [])

  useEffect(() => {
    const d = dialog.current
    if (!d) return
    if (state && !d.open) d.showModal()
    if (!state && d.open) d.close()
  }, [state])

  const finish = (ok: boolean) => {
    const resolve = settle.current
    settle.current = null
    waiting.current = null
    setState(null)
    resolve?.(ok)
    const back = opener.current
    opener.current = null
    queueMicrotask(() => back?.isConnected && back.focus())
  }

  const confirm = async () => {
    setState({ working: true, error: null })
    try {
      await stepUp()
      await qc.invalidateQueries({ queryKey: ['me'] }) // the sidebar mark shows how long it is unlocked
      finish(true)
    } catch (e) {
      setState({ working: false, error: e instanceof Error ? e.message : 'The passkey was not accepted.' })
    }
  }

  return (
    <dialog
      ref={dialog}
      aria-labelledby="stepup-title"
      onCancel={(e) => { e.preventDefault(); if (!state?.working) finish(false) }}
      className="m-auto w-[min(26rem,90vw)] rounded-lg border border-line bg-bg p-5 text-fg backdrop:bg-bg/80"
    >
      {state && (
        <div className="space-y-4">
          <div className="space-y-1.5">
            <h2 id="stepup-title" className="page-title !text-xl">Confirm with your passkey</h2>
            <p className="text-sm text-fg-2">You are away from home. This change needs a passkey confirmation; it then stays unlocked for a few minutes.</p>
          </div>
          {state.error && <Callout kind="error">{state.error}</Callout>}
          <div className="flex justify-end gap-2">
            <button type="button" onClick={() => finish(false)} disabled={state.working} className={`${btn.secondarySm} min-h-9`}>Cancel</button>
            <button type="button" onClick={confirm} disabled={state.working} autoFocus className={`${btn.primarySm} min-h-9`}>
              {state.working ? 'Waiting for your passkey…' : state.error ? 'Try again' : 'Use passkey'}
            </button>
          </div>
        </div>
      )}
    </dialog>
  )
}
