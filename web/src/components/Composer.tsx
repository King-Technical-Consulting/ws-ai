import { useEffect, useRef, useState } from 'react'
import { ArrowUp, Paperclip, Square, X } from 'lucide-react'

export type FilePart = { type: 'file'; mediaType: string; url: string; filename?: string }

export function Composer({
  disabled,
  onSend,
  onStop,
  initialFiles,
}: {
  disabled: boolean
  onSend: (text: string, files?: FilePart[]) => void
  onStop?: () => void
  // Files to start with (a gallery image sent to chat); applied when the
  // array changes.
  initialFiles?: FilePart[]
}) {
  const [text, setText] = useState('')
  const [files, setFiles] = useState<FilePart[]>([])
  const ta = useRef<HTMLTextAreaElement>(null)
  const fileInput = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (initialFiles && initialFiles.length) setFiles((cur) => [...cur, ...initialFiles])
  }, [initialFiles])

  function submit() {
    const t = text.trim()
    if (!t && files.length === 0) return
    onSend(t, files.length ? files : undefined)
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
          </div>
        )}
        <div className="flex items-end gap-2 rounded-2xl border border-line bg-bg-2 px-3 py-2 focus-within:border-accent">
          <button onClick={() => fileInput.current?.click()} className="p-1.5 text-fg-3 hover:text-fg" title="Attach" aria-label="Attach">
            <Paperclip size={18} />
          </button>
          <input ref={fileInput} type="file" multiple hidden onChange={(e) => pick(e.target.files)} accept="image/*,.pdf,.txt,.md,.json,.csv" />
          <textarea
            ref={ta}
            value={text}
            rows={1}
            placeholder="Message…"
            onChange={(e) => {
              setText(e.target.value)
              e.target.style.height = 'auto'
              e.target.style.height = Math.min(e.target.scrollHeight, 240) + 'px'
            }}
            onKeyDown={(e) => {
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
        <p className="text-[11px] text-fg-3 mt-1.5 px-1">Enter to send, Shift+Enter for a new line.</p>
      </div>
    </div>
  )
}
