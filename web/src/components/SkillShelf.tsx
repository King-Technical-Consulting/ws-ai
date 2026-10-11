import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type BulkSkillItem, type SkillDiff, type SkillPreview, type StoredSkill } from '../api'
import { Callout, ListGroup, SectionHead, btn, useConfirm } from './ui'
import { SkillFolderReview } from './SkillFolderReview'

async function postZip<T>(path: string, file: File, dirs: string[] = []): Promise<T> {
  const fd = new FormData()
  fd.append('file', file)
  for (const d of dirs) fd.append('dir', d)
  const res = await fetch(path, { method: 'POST', body: fd, credentials: 'same-origin' })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error((body as { error?: string }).error ?? 'Could not read that archive.')
  return body as T
}

const plural = (n: number, one: string, many = one + 's') => `${n} ${n === 1 ? one : many}`

/** What a re-import changes on the shelf, before anything is replaced. */
export function SkillDiffView({ diff, agents }: { diff: SkillDiff; agents?: string[] }) {
  if (diff.same) return <p className="meta" aria-label="What changes on the shelf">Identical to the skill on the shelf: nothing would change.</p>
  const f = diff.files
  return (
    <div className="space-y-2 text-xs" aria-label="What changes on the shelf">
      {agents && agents.length > 0 && <p className="text-fg-2">Replacing it changes what {plural(agents.length, 'agent')} use: {agents.join(', ')}.</p>}
      {!diff.had_scripts && diff.has_scripts && <Callout kind="error">This version carries scripts; the one on the shelf did not.</Callout>}
      {(diff.title_changed || diff.description_changed) && <p className="text-fg-2">{[diff.title_changed && 'title', diff.description_changed && 'description'].filter(Boolean).join(' and ')} changed.</p>}
      {diff.instructions_changed && (
        <pre className="font-mono whitespace-pre-wrap break-words rounded-lg border border-line p-2 max-h-72 overflow-auto" aria-label="Instructions diff">
          {diff.instructions.map((l, i) => (
            <span key={i} className={l.op === '+' ? 'block text-fg' : l.op === '-' ? 'block text-danger' : 'block text-fg-3'}>
              {l.op} {l.text}
            </span>
          ))}
        </pre>
      )}
      {(f.added.length > 0 || f.removed.length > 0 || f.changed.length > 0) && (
        <ul className="list-disc pl-4">
          {f.added.map((p) => <li key={'a' + p}>added <span className="font-mono">{p}</span></li>)}
          {f.changed.map((p) => <li key={'c' + p}>changed <span className="font-mono">{p}</span></li>)}
          {f.removed.map((p) => <li key={'r' + p}>removed <span className="font-mono">{p}</span></li>)}
        </ul>
      )}
      {diff.scripts_added.length > 0 && <p className="text-danger">Scripts new or changed: <span className="font-mono">{diff.scripts_added.join(', ')}</span></p>}
      {diff.new_findings.length > 0 && <ul className="list-disc pl-4 text-danger">{diff.new_findings.map((x) => <li key={x}>{x}</li>)}</ul>}
    </div>
  )
}

function PreviewCard({ pv, title }: { pv: SkillPreview; title?: string }) {
  return (
    <div className="space-y-2">
      <p className="text-sm">
        {title ?? pv.import.skill.preset.title}
        {pv.existing ? ' is already on the shelf.' : ''}
      </p>
      <SkillFolderReview folder={pv.import} />
      {(pv.import.skill.warnings ?? []).length > 0 && <ul className="list-disc pl-4 text-xs text-fg-2">{pv.import.skill.warnings.map((w) => <li key={w}>{w}</li>)}</ul>}
      {pv.diff && <SkillDiffView diff={pv.diff} agents={pv.agents} />}
    </div>
  )
}

/**
 * The skill shelf (OpenClaw plan, step 2): approved skill folders kept once
 * with their findings and file hashes. An agent picks from here on its own
 * page. A re-import shows a diff before anything is replaced; a skills
 * directory is read folder by folder, each with its own approve or skip.
 * Keeping a skill grants no tool, secret or network access, and nothing runs.
 */
export default function SkillShelf() {
  const qc = useQueryClient()
  const confirmDialog = useConfirm()
  const shelf = useQuery({ queryKey: ['skills'], queryFn: () => api.get<StoredSkill[]>('/api/skills') })
  const oneRef = useRef<HTMLInputElement>(null)
  const bulkRef = useRef<HTMLInputElement>(null)
  const [file, setFile] = useState<File | null>(null)
  const [one, setOne] = useState<SkillPreview | null>(null)
  const [bulkFile, setBulkFile] = useState<File | null>(null)
  const [bulk, setBulk] = useState<BulkSkillItem[] | null>(null)
  const [chosen, setChosen] = useState<Set<string>>(new Set())
  const [error, setError] = useState('')
  const [note, setNote] = useState('')
  const refresh = () => qc.invalidateQueries({ queryKey: ['skills'] })

  const pickOne = async (f: File | undefined) => {
    setError('')
    setNote('')
    setOne(null)
    setFile(null)
    if (!f) return
    try {
      setOne(await postZip<SkillPreview>('/api/skills/preview', f))
      setFile(f)
    } catch (e) {
      setError((e as Error).message)
    }
  }
  const approveOne = useMutation({
    mutationFn: () => postZip<StoredSkill>('/api/skills', file as File),
    onSuccess: (k) => {
      setOne(null)
      setFile(null)
      setNote(`"${k.title}" is on the shelf.`)
      refresh()
    },
    onError: (e) => setError((e as Error).message),
  })
  const pickBulk = async (f: File | undefined) => {
    setError('')
    setNote('')
    setBulk(null)
    setBulkFile(null)
    setChosen(new Set())
    if (!f) return
    try {
      const r = await postZip<{ items: BulkSkillItem[] }>('/api/skills/bulk/preview', f)
      setBulk(r.items)
      setBulkFile(f)
    } catch (e) {
      setError((e as Error).message)
    }
  }
  const approveBulk = useMutation({
    mutationFn: () => postZip<{ approved: StoredSkill[]; failed: BulkSkillItem[] }>('/api/skills/bulk/approve', bulkFile as File, [...chosen]),
    onSuccess: (r) => {
      setNote(`Approved ${plural(r.approved.length, 'skill')}${r.failed.length ? `; ${r.failed.length} could not be stored: ${r.failed.map((f) => `${f.dir} (${f.error})`).join(', ')}` : ''}. The rest were skipped.`)
      setBulk(null)
      setBulkFile(null)
      setChosen(new Set())
      refresh()
    },
    onError: (e) => setError((e as Error).message),
  })
  const remove = useMutation({ mutationFn: (id: string) => api.del(`/api/skills/${id}`), onSuccess: refresh })

  const toggle = (dir: string) =>
    setChosen((c) => {
      const n = new Set(c)
      if (n.has(dir)) n.delete(dir)
      else n.add(dir)
      return n
    })

  const skills = shelf.data ?? []
  return (
    <section className="space-y-3" aria-label="Skill shelf">
      <SectionHead aside={skills.length ? plural(skills.length, 'skill') : undefined}>skill shelf</SectionHead>
      <p className="meta">Skill folders you approved, kept once with what was found in them. An agent picks skills from here; keeping one grants no tool or secret, and nothing in it runs by itself.</p>
      <ListGroup empty={shelf.isLoading ? 'Loading…' : 'The shelf is empty. Add a skill folder, or a whole skills directory from OpenClaw.'}>
        {skills.map((k) => (
          <li key={k.id} className="px-3 py-2 space-y-1">
            <div className="flex items-baseline justify-between gap-3">
              <span className="text-sm">{k.title}</span>
              <button
                onClick={async () => {
                  if (await confirmDialog({ title: `Remove ${k.title} from the shelf?`, body: (k.agent_count ?? 0) > 0 ? `${plural(k.agent_count ?? 0, 'agent')} use it and would lose it.` : 'No agent uses it.', confirm: 'Remove skill', danger: true })) remove.mutate(k.id)
                }}
                disabled={remove.isPending}
                className={btn.danger}
              >
                remove
              </button>
            </div>
            {k.description && <p className="meta">{k.description}</p>}
            <p className="meta">
              {k.has_scripts ? 'has scripts (run only in a code project, through bash)' : 'instructions only'} · {plural(k.files.length, 'file')} · used by {plural(k.agent_count ?? 0, 'agent')} · approved {new Date(k.approved_at).toLocaleDateString()}
            </p>
            {(k.findings.length > 0 || k.refused_secrets.length > 0 || k.files.some((f) => f.findings.length > 0)) && (
              <details className="text-xs text-fg-2">
                <summary className="cursor-pointer">findings and file hashes</summary>
                <ul className="list-disc pl-4">
                  {k.refused_secrets.length > 0 && <li>credentials named, not stored: {k.refused_secrets.join(', ')}</li>}
                  {k.findings.map((f) => <li key={f}>{f}</li>)}
                  {k.files.map((f) => (
                    <li key={f.path}>
                      <span className="font-mono">{f.path}</span> <span className="font-mono text-fg-3">{f.sha256.slice(0, 12)}</span>
                      {f.refused ? ' (not stored)' : ''}
                      {f.findings.length > 0 && <span> — {f.findings.join('; ')}</span>}
                    </li>
                  ))}
                </ul>
              </details>
            )}
          </li>
        ))}
      </ListGroup>

      <input ref={oneRef} type="file" accept=".zip,application/zip" aria-label="Skill folder for the shelf" className="hidden" onChange={(e) => { void pickOne(e.target.files?.[0]); e.target.value = '' }} />
      <input ref={bulkRef} type="file" accept=".zip,application/zip" aria-label="Skills directory for the shelf" className="hidden" onChange={(e) => { void pickBulk(e.target.files?.[0]); e.target.value = '' }} />
      <div className="flex flex-wrap gap-2">
        <button type="button" onClick={() => oneRef.current?.click()} className={btn.secondarySm}>Add a skill folder (.zip)…</button>
        <button type="button" onClick={() => bulkRef.current?.click()} className={btn.secondarySm}>Import a skills directory (.zip)…</button>
      </div>
      {error && <Callout kind="error">{error}</Callout>}
      {note && <Callout kind="note">{note}</Callout>}

      {one && (
        <div className="space-y-2 rounded-lg border border-line p-3" aria-label="Skill to approve">
          <PreviewCard pv={one} />
          <div className="flex gap-2">
            <button onClick={() => approveOne.mutate()} disabled={approveOne.isPending || (one.diff?.same ?? false)} className={btn.primarySm}>
              {one.existing ? 'Replace on the shelf' : 'Approve and add to the shelf'}
            </button>
            <button onClick={() => { setOne(null); setFile(null) }} className={btn.secondarySm}>Cancel</button>
          </div>
        </div>
      )}

      {bulk && (
        <div className="space-y-3 rounded-lg border border-line p-3" aria-label="Skills in the archive">
          <p className="text-sm">{plural(bulk.length, 'skill')} in the archive. Each is read on its own; tick the ones to approve and the rest are skipped.</p>
          {bulk.map((it) => (
            <div key={it.dir} className="space-y-2 border-t border-line pt-2">
              <label className="flex items-center gap-2 text-sm min-h-9">
                <input type="checkbox" checked={chosen.has(it.dir)} disabled={!it.preview} onChange={() => toggle(it.dir)} aria-label={`Approve ${it.dir}`} />
                <span className="font-mono text-xs">{it.dir}</span>
                {it.preview && <span>{it.preview.import.skill.preset.title}</span>}
                {it.preview?.import.has_scripts && <span className="meta">has scripts</span>}
                {it.preview?.diff && !it.preview.diff.same && <span className="meta">changes the shelf</span>}
                {it.preview?.diff?.same && <span className="meta">already on the shelf, unchanged</span>}
              </label>
              {it.error && <Callout kind="error">{it.error}</Callout>}
              {it.preview && (
                <details className="text-xs">
                  <summary className="cursor-pointer text-fg-2">review</summary>
                  <PreviewCard pv={it.preview} />
                </details>
              )}
            </div>
          ))}
          <div className="flex gap-2">
            <button onClick={() => approveBulk.mutate()} disabled={approveBulk.isPending || chosen.size === 0} className={btn.primarySm}>
              Approve {chosen.size} selected
            </button>
            <button onClick={() => { setBulk(null); setBulkFile(null); setChosen(new Set()) }} className={btn.secondarySm}>Cancel</button>
          </div>
        </div>
      )}
    </section>
  )
}
