import { useEffect, useRef, useState } from 'react'
import { ArrowUp, ListChecks, Paperclip, Square, X } from 'lucide-react'
import clsx from 'clsx'

export type FilePart = { type: 'file'; mediaType: string; url: string; filename?: string }

/** A saved prompt offered in the slash list. */
export type SavedPromptItem = { id: string; name: string; text: string }

type SlashItem = { id: string; label: string; hint: string; kind: 'plan' | 'remember' | 'schedule' | 'notebook' | 'prompt'; text?: string }

const COMMANDS: SlashItem[] = [
  { id: 'plan', label: '/plan', hint: 'Write a plan for me to approve before anything runs', kind: 'plan' },
  { id: 'remember', label: '/remember', hint: 'Keep something about me: /remember I prefer metric units', kind: 'remember' },
  { id: 'schedule', label: '/schedule', hint: 'Run this prompt later or on a repeat', kind: 'schedule' },
  { id: 'notebook', label: '/notebook', hint: 'Open my notebooks', kind: 'notebook' },
]

export function Composer({
  disabled,
  onSend,
  onStop,
  initialFiles,
  imageNote,
  plan = false,
  onPlan,
  savedPrompts = [],
  onCommand,
}: {
  // The member's saved prompts, offered after the commands in the "/" list.
  savedPrompts?: SavedPromptItem[]
  // Runs /remember, /schedule and /notebook; returns a short result line to
  // show under the box (or nothing). The composer clears the text first.
  onCommand?: (name: 'remember' | 'schedule' | 'notebook', arg: string) => Promise<string | void> | string | void
  disabled: boolean
  onSend: (text: string, files?: FilePart[], opts?: { plan?: boolean }) => void
  // Plan mode (M13): the model writes a plan the person approves before any
  // tool runs. On for the next message only; `/plan` and Shift+Tab set it.
  plan?: boolean
  onPlan?: (on: boolean) => void
  onStop?: () => void
  // Files to start with (a gallery image sent to chat); applied when the
  // array changes.
  initialFiles?: FilePart[]
  // Shown next to the attachments while an image is among them: the model
  // picked cannot read images (Chat works it out from the picker's value).
  imageNote?: string
}) {
  const [text, setText] = useState('')
  const [files, setFiles] = useState<FilePart[]>([])
  const [slashIndex, setSlashIndex] = useState(0)
  const [slashDismissed, setSlashDismissed] = useState(false)
  const [note, setNote] = useState('')
  const ta = useRef<HTMLTextAreaElement>(null)
  const fileInput = useRef<HTMLInputElement>(null)

  // Opening a conversation puts the cursor in the box, so typing goes straight in. The Composer is
  // mounted afresh for each conversation, so mounting is opening. Not on a phone (below md, or a
  // touch screen), where it would raise the keyboard over the chat; not over a dialog; and not when a
  // field already has the focus (the rename field, say).
  useEffect(() => {
    const wide = window.matchMedia('(min-width: 768px) and (pointer: fine)')
    const editable = (el: Element | null) =>
      !!el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || (el as HTMLElement).isContentEditable)
    if (wide.matches && !document.querySelector('dialog[open]') && !editable(document.activeElement)) ta.current?.focus({ preventScroll: true })

    // A printable key typed while nothing has the focus goes to the box and is kept: the focus moves
    // during the keydown, so the character is typed into the box.
    const onKey = (e: KeyboardEvent) => {
      if (e.key.length !== 1 || e.key === ' ' || e.ctrlKey || e.metaKey || e.altKey || e.defaultPrevented) return
      const a = document.activeElement
      if ((a && a !== document.body) || document.querySelector('dialog[open]') || !wide.matches) return
      if (ta.current && !ta.current.disabled) ta.current.focus({ preventScroll: true })
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])
  useEffect(() => {
    if (initialFiles && initialFiles.length) setFiles((cur) => [...cur, ...initialFiles])
  }, [initialFiles])

  // The "/" list: shown while the text is only a command word.
  const word = /^\/(\S*)$/.exec(text)
  const slashItems: SlashItem[] = word
    ? [
        ...COMMANDS.filter((c) => c.id.startsWith(word[1].toLowerCase())),
        ...savedPrompts
          .filter((p) => p.name.toLowerCase().includes(word[1].toLowerCase()))
          .map((p): SlashItem => ({ id: `p-${p.id}`, label: p.name, hint: p.text.slice(0, 80), kind: 'prompt', text: p.text })),
      ].filter((c) => c.kind !== 'plan' || !!onPlan)
    : []
  const slashOpen = !!word && !slashDismissed && slashItems.length > 0
  const active = Math.min(slashIndex, Math.max(slashItems.length - 1, 0))

  function choose(item: SlashItem) {
    setSlashDismissed(false)
    setSlashIndex(0)
    switch (item.kind) {
      case 'prompt':
        setText(item.text ?? '')
        break
      case 'plan':
        onPlan?.(true)
        setText('')
        break
      case 'remember':
        setText('/remember ')
        break
      default:
        setText('')
        void Promise.resolve(onCommand?.(item.kind, '')).then((r) => setNote(r || ''))
    }
    ta.current?.focus()
  }

  function submit() {
    let t = text.trim()
    const cmd = /^\/(remember|schedule|notebook)(?:\s+([\s\S]*))?$/.exec(t)
    if (cmd && onCommand) {
      setText('')
      void Promise.resolve(onCommand(cmd[1] as 'remember' | 'schedule' | 'notebook', (cmd[2] ?? '').trim())).then((r) => setNote(r || ''))
      return
    }
    let planned = plan
    const slash = /^\/plan(?:\s+|$)/.exec(t)
    if (slash && onPlan) {
      // "/plan" alone arms plan mode; "/plan do the thing" sends it planned.
      t = t.slice(slash[0].length).trim()
      planned = true
      if (!t && files.length === 0) {
        onPlan(true)
        setText('')
        return
      }
    }
    if (!t && files.length === 0) return
    onSend(t, files.length ? files : undefined, planned ? { plan: true } : undefined)
    setText('')
    setFiles([])
    if (ta.current) ta.current.style.height = 'auto'
  }

  async function pick(list: FileList | null) {
    if (!list) return
    const parts: FilePart[] = []
    for (const f of Array.from(list)) {
      if (f.size > 20 * 1024 * 1024) continue
      const url = await new Promise<string>((res) => {
        const r = new FileReader()
        r.onload = () => res(r.result as string)
        r.readAsDataURL(f)
      })
      parts.push({ type: 'file', mediaType: f.type || 'application/octet-stream', url, filename: f.name })
    }
    setFiles((cur) => [...cur, ...parts])
  }

  return (
    <div className="shrink-0 border-t border-line bg-bg">
      <div className="mx-auto max-w-3xl p-3">
        {files.length > 0 && (
          <div className="flex flex-wrap gap-2 mb-2">
            {files.map((f, i) => (
              <span key={i} className="inline-flex items-center gap-1 rounded-md bg-bg-3 px-2 py-1 text-xs">
                {f.filename}
                <button onClick={() => setFiles(files.filter((_, j) => j !== i))} className="text-fg-2 hover:text-fg" aria-label={`Remove ${f.filename}`}>
                  <X size={12} />
                </button>
              </span>
            ))}
            {imageNote && files.some((f) => f.mediaType.startsWith('image/')) && (
              <span role="note" className="meta self-center">{imageNote}</span>
            )}
          </div>
        )}
        {slashOpen && (
          <ul id="slash-list" role="listbox" aria-label="Commands and saved prompts" className="mb-2 max-h-56 overflow-auto rounded-lg border border-line bg-bg-2 text-sm">
            {slashItems.map((it, i) => (
              <li
                key={it.id}
                id={`slash-${it.id}`}
                role="option"
                aria-selected={i === active}
                onMouseDown={(e) => {
                  e.preventDefault()
                  choose(it)
                }}
                className={clsx('flex items-baseline gap-3 px-3 py-1.5 cursor-pointer', i === active ? 'bg-bg-3' : 'hover:bg-bg-3')}
              >
                <span className={clsx('shrink-0', it.kind === 'prompt' ? 'text-fg' : 'font-mono text-xs text-accent')}>{it.label}</span>
                <span className="meta truncate">{it.hint}</span>
              </li>
            ))}
          </ul>
        )}
        <div className="flex items-end gap-2 rounded-2xl border border-line bg-bg-2 px-3 py-2 focus-within:border-accent">
          <button onClick={() => fileInput.current?.click()} className="p-1.5 text-fg-3 hover:text-fg" title="Attach" aria-label="Attach">
            <Paperclip size={18} />
          </button>
          {onPlan && (
            <button
              onClick={() => onPlan(!plan)}
              className={clsx('p-1.5 rounded-md hover:bg-bg-3', plan ? 'text-accent' : 'text-fg-3 hover:text-fg')}
              title="Plan first (Shift+Tab)"
              aria-label="Plan first"
              aria-pressed={plan}
            >
              <ListChecks size={18} />
            </button>
          )}
          <input ref={fileInput} type="file" multiple hidden onChange={(e) => pick(e.target.files)} accept="image/*,.pdf,.docx,.xlsx,.pptx,.txt,.md,.json,.csv,.tsv" />
          <textarea
            ref={ta}
            value={text}
            rows={1}
            placeholder="Message…"
            onChange={(e) => {
              setNote('')
              setSlashDismissed(false)
              setSlashIndex(0)
              setText(e.target.value)
              e.target.style.height = 'auto'
              e.target.style.height = Math.min(e.target.scrollHeight, 240) + 'px'
            }}
            aria-controls={slashOpen ? 'slash-list' : undefined}
            aria-activedescendant={slashOpen ? `slash-${slashItems[active].id}` : undefined}
            onKeyDown={(e) => {
              if (slashOpen) {
                if (e.key === 'ArrowDown') {
                  e.preventDefault()
                  setSlashIndex((active + 1) % slashItems.length)
                  return
                }
                if (e.key === 'ArrowUp') {
                  e.preventDefault()
                  setSlashIndex((active - 1 + slashItems.length) % slashItems.length)
                  return
                }
                if ((e.key === 'Enter' && !e.shiftKey) || e.key === 'Tab') {
                  e.preventDefault()
                  choose(slashItems[active])
                  return
                }
                if (e.key === 'Escape') {
                  e.preventDefault()
                  setSlashDismissed(true)
                  return
                }
              }
              if (e.key === 'Tab' && e.shiftKey && onPlan) {
                e.preventDefault()
                onPlan(!plan)
                return
              }
              if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault()
                if (!disabled) submit()
              }
            }}
            className="flex-1 resize-none bg-transparent outline-none py-1 max-h-60 reading"
          />
          {onStop ? (
            <button onClick={onStop} className="p-1.5 rounded-full bg-fg text-bg" title="Stop" aria-label="Stop">
              <Square size={16} />
            </button>
          ) : (
            <button
              onClick={submit}
              disabled={disabled || (!text.trim() && files.length === 0)}
              className="p-1.5 rounded-full bg-accent text-accent-fg hover:bg-accent-strong disabled:opacity-40"
              title="Send"
              aria-label="Send"
            >
              <ArrowUp size={16} />
            </button>
          )}
        </div>
        {note && <p role="status" className="meta mt-1.5 px-1">{note}</p>}
        <p className="text-[11px] text-fg-3 mt-1.5 px-1">
          {plan ? 'Plan first is on: the model writes a plan for you to approve before it runs anything. ' : ''}
          Enter to send, Shift+Enter for a new line, / for commands and saved prompts, Shift+Tab to plan first.
        </p>
      </div>
    </div>
  )
}
