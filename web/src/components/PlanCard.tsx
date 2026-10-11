import { useState } from 'react'
import clsx from 'clsx'
import { Check, Minus } from 'lucide-react'
import { SectionHead, btn, inputSm } from './ui'

export type PlanInput = { summary?: string; steps?: { text?: string; tools?: string[] }[] }
export type StepMarks = Record<number, 'done' | 'skipped'>
export type OnPlanDecision = (approvalId: string, approved: boolean, reason?: string) => void

const EDITED = 'The plan was approved with changes.'

/** The plan the person approved with changes, from the tool's result text; null when it was approved as written. */
export function editedPlan(output: unknown): string | null {
  if (typeof output !== 'string' || !output.startsWith(EDITED)) return null
  const i = output.indexOf('\n\n')
  return i < 0 ? null : output.slice(i + 2).trim()
}

/** One step per non-empty line, numbering and bullets stripped. */
export function parsePlanLines(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.replace(/^\s*(?:\d+[.)]|[-*•])\s*/, '').trim())
    .filter(Boolean)
}

function planText(input: PlanInput): string {
  return (input.steps ?? []).map((s, i) => `${i + 1}. ${s.text ?? ''}`).join('\n')
}

/**
 * The model's written plan (the submit_plan tool call). While it waits for
 * a decision the person can approve it, edit it and approve, or reject it;
 * afterwards it is a checklist that ticks as the model reports steps done.
 */
export function PlanCard({
  input,
  state,
  output,
  approvalId,
  marks,
  onDecision,
}: {
  input: PlanInput
  state: string
  output?: unknown
  approvalId?: string
  marks: StepMarks
  onDecision?: OnPlanDecision
}) {
  const [editing, setEditing] = useState(false)
  const [text, setText] = useState(() => planText(input))
  const [reason, setReason] = useState('')
  const [rejecting, setRejecting] = useState(false)
  const waiting = state === 'approval-requested' && !!approvalId
  const rejected = state === 'output-error'
  const edited = editedPlan(output)
  const steps = edited ? parsePlanLines(edited).map((t) => ({ text: t })) : (input.steps ?? [])
  const changed = parsePlanLines(text).join('\n') !== (input.steps ?? []).map((s) => s.text ?? '').join('\n')

  return (
    <section aria-label="Plan" className="my-3 rounded-lg border border-line bg-bg-2 font-sans text-sm">
      <div className="px-3 pt-2">
        <SectionHead aside={waiting ? 'waiting for you' : rejected ? 'rejected' : edited ? 'approved with changes' : state === 'output-available' ? 'approved' : undefined}>plan</SectionHead>
        {input.summary && <p className="reading-tight mt-2">{input.summary}</p>}
      </div>
      {editing && waiting ? (
        <div className="px-3 py-2">
          <label className="meta" htmlFor={`plan-edit-${approvalId}`}>One step per line.</label>
          <textarea id={`plan-edit-${approvalId}`} value={text} onChange={(e) => setText(e.target.value)} rows={Math.max(4, text.split('\n').length + 1)} className={clsx(inputSm, 'mt-1 font-mono text-xs')} />
        </div>
      ) : (
        <ol className="px-3 py-2 space-y-1.5">
          {steps.map((s, i) => {
            const mark = marks[i + 1]
            return (
              <li key={i} className="flex items-start gap-2" data-step-state={mark ?? 'open'}>
                <span aria-hidden className={clsx('mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded-md border', mark === 'done' ? 'border-accent text-accent' : 'border-line text-fg-3')}>
                  {mark === 'done' ? <Check size={11} /> : mark === 'skipped' ? <Minus size={11} /> : null}
                </span>
                <span className={clsx(mark && 'text-fg-3', mark === 'done' && 'line-through')}>
                  {s.text}
                  {'tools' in s && s.tools && s.tools.length > 0 && <span className="meta ml-2">{s.tools.join(', ')}</span>}
                </span>
                {mark && <span className="sr-only">{mark}</span>}
              </li>
            )
          })}
        </ol>
      )}
      {waiting && (
        <div className="border-t border-line px-3 py-2 space-y-2">
          {rejecting && (
            <input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Why, or what to do instead (optional)" aria-label="Reason for rejecting" className={inputSm} />
          )}
          <div className="flex flex-wrap gap-2">
            {editing ? (
              <>
                <button className={btn.primarySm} onClick={() => onDecision?.(approvalId!, true, changed ? parsePlanLines(text).map((t, i) => `${i + 1}. ${t}`).join('\n') : undefined)}>
                  {changed ? 'Approve edited plan' : 'Approve'}
                </button>
                <button className={btn.secondarySm} onClick={() => { setEditing(false); setText(planText(input)) }}>Cancel edit</button>
              </>
            ) : rejecting ? (
              <>
                <button className={btn.primarySm} onClick={() => onDecision?.(approvalId!, false, reason.trim() || undefined)}>Reject plan</button>
                <button className={btn.secondarySm} onClick={() => setRejecting(false)}>Back</button>
              </>
            ) : (
              <>
                <button className={btn.primarySm} onClick={() => onDecision?.(approvalId!, true)}>Approve</button>
                <button className={btn.secondarySm} onClick={() => setEditing(true)}>Edit</button>
                <button className={btn.secondarySm} onClick={() => setRejecting(true)}>Reject</button>
              </>
            )}
          </div>
        </div>
      )}
    </section>
  )
}

/** Step marks from the plan_step calls in one assistant message's parts. */
export function stepMarks(parts: { type: string; input?: unknown }[]): StepMarks {
  const out: StepMarks = {}
  for (const p of parts) {
    if (p.type !== 'tool-plan_step') continue
    const inp = (p.input ?? {}) as { step?: number; status?: string }
    if (typeof inp.step === 'number' && (inp.status === 'done' || inp.status === 'skipped')) out[inp.step] = inp.status
  }
  return out
}
