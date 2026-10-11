import { useRef, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import clsx from 'clsx'
import { api, type Dataset, type DatasetMapping } from '../api'
import { SectionHead, Callout, btn, inputSm } from './ui'

const ACCEPT = '.jsonl,.ndjson,.csv,.tsv,.txt,.md,.markdown,.zip,.tgz,.tar,.tar.gz'

/**
 * Upload a dataset of your own (PLAN M12): a JSONL, CSV, TSV, text or
 * markdown file, or a zip or tar.gz of them. Records are read through a
 * column mapping; leave it empty to detect the common names (prompt,
 * question, completion, answer, ...).
 */
export function DatasetUpload({ onChange }: { onChange: () => void }) {
  const fileRef = useRef<HTMLInputElement>(null)
  const [file, setFile] = useState<File | null>(null)
  const [name, setName] = useState('')
  const [holdout, setHoldout] = useState(10)
  const [mapping, setMapping] = useState<DatasetMapping>({})
  const upload = useMutation({
    mutationFn: () => {
      const fd = new FormData()
      fd.append('name', name || file!.name)
      fd.append('holdout_pct', String(holdout))
      fd.append('mapping', JSON.stringify(clean(mapping)))
      fd.append('file', file!) // the file goes last: the server reads the form as a stream
      return api.postRaw<Dataset>('/api/training/datasets/upload', fd)
    },
    onSuccess: () => {
      setFile(null)
      setName('')
      if (fileRef.current) fileRef.current.value = ''
      onChange()
    },
  })
  return (
    <section className="space-y-3">
      <SectionHead>upload a dataset</SectionHead>
      <form
        className="space-y-2 rounded-lg border border-line bg-bg-2 p-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (file) upload.mutate()
        }}
      >
        <div className="flex flex-wrap items-center gap-2">
          <input ref={fileRef} type="file" accept={ACCEPT} aria-label="Dataset file" onChange={(e) => setFile(e.target.files?.[0] ?? null)} className="text-xs text-fg-2" />
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder={file ? file.name : 'Name (the file name if empty)'} className={clsx(inputSm, 'flex-1 min-w-48')} aria-label="Uploaded dataset name" />
          <label className="flex items-center gap-1 text-xs text-fg-2">
            held out <input type="number" min={0} max={50} value={holdout} onChange={(e) => setHoldout(Number(e.target.value))} className={clsx(inputSm, 'w-16 tnum')} aria-label="Uploaded held-out percent" /> %
          </label>
        </div>
        <MappingFields value={mapping} onChange={setMapping} />
        <div className="flex items-center gap-3">
          <button type="submit" disabled={upload.isPending || !file} className={btn.primarySm}>{upload.isPending ? 'Uploading…' : 'Upload'}</button>
          <span className="meta">.jsonl, .csv, .tsv, .txt, .md, or a .zip or .tar.gz of them. A chat-format JSONL line ({'{"messages":[…]}'}) is taken as is.</span>
        </div>
        {upload.error && <Callout kind="error">{upload.error.message}</Callout>}
      </form>
    </section>
  )
}

function clean(m: DatasetMapping): DatasetMapping {
  const out: DatasetMapping = {}
  for (const [k, v] of Object.entries(m)) if (v !== undefined && v !== '' && v !== 0) (out as Record<string, unknown>)[k] = v
  return out
}

function MappingFields({ value, onChange, columns }: { value: DatasetMapping; onChange: (m: DatasetMapping) => void; columns?: string[] }) {
  const set = (k: keyof DatasetMapping, v: string) => onChange({ ...value, [k]: k === 'chunk_chars' ? Number(v) : v })
  const field = (k: keyof DatasetMapping, label: string, hint: string) => (
    <label className="flex flex-col gap-1 text-xs text-fg-2">
      {label}
      <input value={(value[k] as string | number | undefined) ?? ''} onChange={(e) => set(k, e.target.value)} placeholder={hint} list={columns ? 'dataset-columns' : undefined} className={clsx(inputSm, 'font-mono')} aria-label={label} />
    </label>
  )
  return (
    <div className="space-y-2">
      {columns && <datalist id="dataset-columns">{columns.map((c) => <option key={c} value={c} />)}</datalist>}
      <div className="grid gap-2 sm:grid-cols-3">
        {field('input', 'input column', 'detected: prompt, question, …')}
        {field('output', 'output column', 'detected: completion, answer, …')}
        {field('system', 'system column', 'optional, per record')}
        {field('split', 'split column', 'optional: train or eval')}
        {field('chunk_chars', 'document chunk size', '3000 (200 to 20000)')}
      </div>
      <label className="flex flex-col gap-1 text-xs text-fg-2">
        system prompt for every record
        <input value={value.system_text ?? ''} onChange={(e) => set('system_text', e.target.value)} placeholder="optional" className={inputSm} aria-label="System prompt for every record" />
      </label>
    </div>
  )
}

/** What the last build found in an uploaded dataset, and its mapping, editable (saving rebuilds). */
export function UploadedDetails({ dataset, onChange }: { dataset: Dataset; onChange: () => void }) {
  const r = dataset.report
  const [mapping, setMapping] = useState<DatasetMapping>(dataset.mapping ?? {})
  const save = useMutation({ mutationFn: () => api.put<Dataset>(`/api/training/datasets/${dataset.id}/mapping`, clean(mapping)), onSuccess: onChange })
  const settled = dataset.status === 'ready' || dataset.status === 'failed'
  return (
    <div className="space-y-2 rounded-lg border border-line bg-bg-2 p-3 text-sm">
      <p className="meta">
        {dataset.source_name} · {r.train ?? 0} train, {r.eval ?? 0} held out
        {r.mapping?.input ? ` · input “${r.mapping.input}”, output “${r.mapping.output ?? ''}”` : ''}
        {r.truncated ? ' · stopped at the example cap' : ''}
      </p>
      {r.files && r.files.length > 0 && (
        <ul className="meta space-y-0.5">
          {r.files.map((f) => (
            <li key={f.name}>
              {f.name} · {f.kind} · {f.examples} examples{f.skipped ? `, ${f.skipped} skipped` : ''}
            </li>
          ))}
          {r.files_omitted ? <li>and {r.files_omitted} more files</li> : null}
        </ul>
      )}
      {r.skipped_types && Object.keys(r.skipped_types).length > 0 && (
        <p className="meta">Not read: {Object.entries(r.skipped_types).map(([ext, n]) => `${n} ${ext}`).join(', ')}</p>
      )}
      {r.warnings?.map((w, i) => <p key={i} className="meta">{w}</p>)}
      {r.columns && r.columns.length > 0 && <p className="meta">Columns found: {r.columns.join(', ')}</p>}
      <MappingFields value={mapping} onChange={setMapping} columns={r.columns} />
      <div className="flex items-center gap-3">
        <button onClick={() => save.mutate()} disabled={!settled || save.isPending} className={btn.primarySm}>Save mapping and rebuild</button>
        {!settled && <span className="meta">The build is still running.</span>}
      </div>
      {save.error && <Callout kind="error">{save.error.message}</Callout>}
    </div>
  )
}
