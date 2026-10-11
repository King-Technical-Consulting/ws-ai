// In-app confirm and prompt dialogs, so nothing in the app opens the browser's own
// confirm(), prompt() or alert(). A native <dialog> shown with showModal(): the browser
// traps focus and puts the rest of the page out of reach. Call sites read
//   if (await confirm({ title: 'Delete adapter?', confirm: 'Delete adapter', danger: true })) remove.mutate()
//   const name = await prompt({ title: 'Design project', label: 'Name', initial: 'Design', confirm: 'Create' })
// Escape, Cancel and a click on the backdrop cancel (false / null); Enter confirms; focus goes
// back to whatever opened the dialog.
import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { btn, inputSm } from './ui'

export type ConfirmOptions = {
  title: string
  /** One sentence on what happens. */
  body?: string
  /** The verb on the primary button ("Delete adapter"), never "OK". */
  confirm: string
  /** Destructive: the primary button is drawn in the danger colour. */
  danger?: boolean
}

export type PromptOptions = {
  title: string
  body?: string
  label: string
  initial?: string
  confirm: string
  /** Destructive: the primary button is drawn in the danger colour. */
  danger?: boolean
  /** The answer must equal this exactly (a typed confirmation): the button stays disabled until it does. */
  match?: string
}

type Request =
  | { kind: 'confirm'; opts: ConfirmOptions; resolve: (v: boolean) => void }
  | { kind: 'prompt'; opts: PromptOptions; resolve: (v: string | null) => void }

type Dialogs = {
  confirm: (o: ConfirmOptions) => Promise<boolean>
  prompt: (o: PromptOptions) => Promise<string | null>
}

const Ctx = createContext<Dialogs | null>(null)

export function useConfirm() {
  const d = useContext(Ctx)
  if (!d) throw new Error('useConfirm needs a DialogProvider')
  return d.confirm
}

export function usePrompt() {
  const d = useContext(Ctx)
  if (!d) throw new Error('usePrompt needs a DialogProvider')
  return d.prompt
}

const dangerBtn = 'inline-flex items-center justify-center gap-2 shrink-0 whitespace-nowrap rounded-lg border border-danger text-danger px-4 py-2 text-sm font-medium hover:bg-bg-2 disabled:opacity-60'

export function DialogProvider({ children }: { children: React.ReactNode }) {
  const [req, setReq] = useState<Request | null>(null)
  const [text, setText] = useState('')
  const dialog = useRef<HTMLDialogElement>(null)
  const opener = useRef<HTMLElement | null>(null)
  const pending = useRef<Request | null>(null)

  const open = useCallback(<T,>(make: (resolve: (v: T) => void) => Request) => new Promise<T>((resolve) => {
    // A second request while one is open cancels the first, so no promise is left hanging.
    pending.current?.resolve(pending.current.kind === 'confirm' ? (false as never) : (null as never))
    const r = make(resolve)
    pending.current = r
    opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    if (r.kind === 'prompt') setText(r.opts.initial ?? '')
    setReq(r)
  }), [])

  const api = useRef<Dialogs>({
    confirm: (opts) => open<boolean>((resolve) => ({ kind: 'confirm', opts, resolve })),
    prompt: (opts) => open<string | null>((resolve) => ({ kind: 'prompt', opts, resolve })),
  })

  useEffect(() => {
    const d = dialog.current
    if (!d) return
    if (req && !d.open) d.showModal()
    if (!req && d.open) d.close()
  }, [req])

  const finish = (value: boolean | string | null) => {
    const r = pending.current
    if (!r) return
    pending.current = null
    setReq(null)
    if (r.kind === 'confirm') r.resolve(value === true)
    else r.resolve(typeof value === 'string' ? value : null)
    const back = opener.current
    opener.current = null
    // After the dialog has closed, so focus is not pulled back into it.
    queueMicrotask(() => back?.isConnected && back.focus())
  }

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!req) return
    if (req.kind === 'confirm') finish(true)
    else if (text.trim()) finish(text.trim())
  }

  return (
    <Ctx.Provider value={api.current}>
      {children}
      <dialog
        ref={dialog}
        aria-labelledby="dialog-title"
        onCancel={(e) => { e.preventDefault(); finish(req?.kind === 'confirm' ? false : null) }}
        onClick={(e) => { if (e.target === dialog.current) finish(req?.kind === 'confirm' ? false : null) }}
        className="m-auto w-[min(26rem,90vw)] rounded-lg border border-line bg-bg p-5 text-fg backdrop:bg-bg/80"
      >
        {req && (
          <form onSubmit={submit} className="space-y-4">
            <div className="space-y-1.5">
              <h2 id="dialog-title" className="page-title !text-xl">{req.opts.title}</h2>
              {req.opts.body && <p className="text-sm text-fg-2">{req.opts.body}</p>}
            </div>
            {req.kind === 'prompt' && (
              <label className="block text-sm text-fg-2">
                {req.opts.label}
                <input
                  value={text}
                  onChange={(e) => setText(e.target.value)}
                  onFocus={(e) => e.currentTarget.select()}
                  className={`${inputSm} mt-1 min-h-9`}
                  maxLength={200}
                  autoFocus
                />
              </label>
            )}
            <div className="flex justify-end gap-2">
              <button type="button" onClick={() => finish(req.kind === 'confirm' ? false : null)} className={`${btn.secondarySm} min-h-9`}>Cancel</button>
              <button
                type="submit"
                autoFocus={req.kind === 'confirm'}
                disabled={req.kind === 'prompt' && (!text.trim() || (req.opts.match !== undefined && text.trim() !== req.opts.match))}
                className={req.opts.danger ? `${dangerBtn} min-h-9` : `${btn.primarySm} min-h-9`}
              >
                {req.opts.confirm}
              </button>
            </div>
          </form>
        )}
      </dialog>
    </Ctx.Provider>
  )
}
