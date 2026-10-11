import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { api, type Dataset, type FinetuneJob, type FinetuneTarget, type Recipe, type RecipeSpec, type SpecMapping } from '../api'
import { SectionHead, Callout, ListGroup, Row, btn, inputSm, useConfirm } from './ui'
import { BaseModelPicker } from './BaseModelPicker'
import { ago } from '../pages/Agents'
import { isSmokeEval } from '../lib/evalSize'

/**
 * Recipes (PLAN M12): a base model, your instructions in plain words and the
 * datasets attached to them. Flesh out has a model turn those into a spec
 * (system prompt, column mappings, eval rubric, hyperparameters) you read and
 * correct; a key you edit by hand is kept when you flesh out again.
 */
export function Recipes({ datasets, smoke }: { datasets: Dataset[]; smoke: number }) {
  const qc = useQueryClient()
  const confirmDialog = useConfirm()
  const recipes = useQuery({
    queryKey: ['training', 'recipes'],
    queryFn: () => api.get<{ recipes: Recipe[] }>('/api/training/recipes'),
    refetchInterval: (q) => (q.state.data?.recipes.some((r) => r.spec_status === 'queued' || r.spec_status === 'running') ? 2500 : false),
  })
  const refresh = () => qc.invalidateQueries({ queryKey: ['training', 'recipes'] })
  const [name, setName] = useState('')
  const [base, setBase] = useState('')
  const [openID, setOpenID] = useState<string | null>(null)
  const create = useMutation({
    mutationFn: () => api.post<Recipe>('/api/training/recipes', { name, base_model: base }),
    onSuccess: (r) => {
      setName('')
      setOpenID(r.id)
      refresh()
    },
  })
  const remove = useMutation({ mutationFn: (id: string) => api.del(`/api/training/recipes/${id}`), onSuccess: refresh })
  const rows = recipes.data?.recipes ?? []
  return (
    <section className="space-y-3">
      <SectionHead aside={rows.length ? `${rows.length}` : undefined}>recipes</SectionHead>
      <p className="text-sm text-fg-2">
        Say what a model should learn to do, attach the datasets you have, and let a model write the training spec for you to review. Nothing trains from a recipe yet.
      </p>
      <ListGroup empty="No recipes yet.">
        {rows.map((r) => (
          <Row
            key={r.id}
            action={
              <span className="flex flex-wrap items-center gap-x-3">
                <button onClick={() => setOpenID(openID === r.id ? null : r.id)} className={btn.link}>
                  {openID === r.id ? 'close' : 'open'}
                </button>
                <button
                  onClick={async () => {
                    if (await confirmDialog({ title: `Delete recipe "${r.name}"?`, body: 'The recipe and its spec are removed. Datasets and adapters stay.', confirm: 'Delete recipe', danger: true })) remove.mutate(r.id)
                  }}
                  className={btn.danger}
                >
                  remove
                </button>
              </span>
            }
          >
            <span className="flex flex-wrap items-baseline gap-x-2">
              <SpecWord s={r.spec_status} />
              {r.name}
            </span>
            <span className="meta block">
              {r.base_model} · {r.dataset_ids.length} {r.dataset_ids.length === 1 ? 'dataset' : 'datasets'} · {ago(r.updated_at)}
            </span>
          </Row>
        ))}
      </ListGroup>
      {rows.filter((r) => r.id === openID).map((r) => (
        <RecipeEditor key={r.id + r.updated_at} recipe={r} datasets={datasets} smoke={smoke} onChange={refresh} />
      ))}
      <form
        className="flex flex-wrap gap-2 rounded-lg border border-line bg-bg-2 p-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (name.trim() && base.trim()) create.mutate()
        }}
      >
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Recipe name" aria-label="Recipe name" className={clsx(inputSm, 'flex-1 min-w-48')} />
        <BaseModelPicker value={base} onChange={setBase} label="Recipe model id" />
        <button type="submit" disabled={create.isPending || !name.trim() || !base.trim()} className={btn.primarySm}>New recipe</button>
        {create.error && <div className="w-full"><Callout kind="error">{create.error.message}</Callout></div>}
      </form>
    </section>
  )
}

function SpecWord({ s }: { s: Recipe['spec_status'] }) {
  const label = s === 'none' ? 'draft' : s === 'ready' ? 'spec' : s
  const open = s === 'queued' || s === 'running'
  return <span className={clsx('section-head border-0 shrink-0', open ? 'text-fg' : s === 'failed' ? 'text-danger' : 'text-fg-3')}>{label}</span>
}

function RecipeEditor({ recipe, datasets, smoke, onChange }: { recipe: Recipe; datasets: Dataset[]; smoke: number; onChange: () => void }) {
  const [name, setName] = useState(recipe.name)
  const [base, setBase] = useState(recipe.base_model)
  const [instructions, setInstructions] = useState(recipe.instructions)
  const [ids, setIds] = useState<string[]>(recipe.dataset_ids)
  const [model, setModel] = useState(recipe.spec_model)
  const [replace, setReplace] = useState(false)
  const [showPrompt, setShowPrompt] = useState(false)
  const dirty = name !== recipe.name || base !== recipe.base_model || instructions !== recipe.instructions || model !== recipe.spec_model || ids.join() !== recipe.dataset_ids.join()
  const save = useMutation({
    mutationFn: () => api.put<Recipe>(`/api/training/recipes/${recipe.id}`, { name, base_model: base, base_endpoint_id: recipe.base_endpoint_id ?? '', instructions, dataset_ids: ids, target: recipe.target, config: recipe.config, spec_model: model }),
    onSuccess: onChange,
  })
  const flesh = useMutation({
    mutationFn: () => api.post<Recipe>(`/api/training/recipes/${recipe.id}/flesh-out`, { model, replace }),
    onSuccess: onChange,
  })
  // The eval of a recipe's adapter runs on every attached dataset's held-out split together.
  const readySets = datasets.filter((d) => ids.includes(d.id) && d.status === 'ready')
  const heldOut = { datasets: readySets.length, examples: readySets.reduce((n, d) => n + d.eval_examples, 0) }
  const working = recipe.spec_status === 'queued' || recipe.spec_status === 'running'
  const hasSpec = Object.keys(recipe.spec ?? {}).length > 0
  return (
    <div className="space-y-3 rounded-lg border border-line bg-bg-2 p-3 text-sm">
      <div className="flex flex-wrap gap-2">
        <input value={name} onChange={(e) => setName(e.target.value)} aria-label="Name" className={clsx(inputSm, 'flex-1 min-w-48')} />
        <BaseModelPicker value={base} onChange={setBase} label="Editor model id" />
      </div>
      <label className="block space-y-1">
        <span className="section-head border-0">instructions</span>
        <textarea
          value={instructions}
          onChange={(e) => setInstructions(e.target.value)}
          rows={6}
          placeholder="What should the model learn to do? For whom, in what tone, and what must it never do?"
          aria-label="Instructions"
          className={clsx(inputSm, 'reading')}
        />
      </label>
      <fieldset className="space-y-1">
        <legend className="section-head border-0">datasets</legend>
        {datasets.length === 0 && <p className="meta">No datasets yet: build one from conversations or upload your own above.</p>}
        {datasets.map((d) => (
          <label key={d.id} className="flex items-center gap-2 text-xs text-fg-2">
            <input type="checkbox" className="accent-accent" checked={ids.includes(d.id)} onChange={(e) => setIds(e.target.checked ? [...ids, d.id] : ids.filter((x) => x !== d.id))} />
            <span className="text-fg">{d.name}</span>
            <span className="meta">{d.status === 'ready' ? `${d.examples} examples` : d.status}{d.kind === 'uploaded' ? ' · uploaded' : ''}</span>
          </label>
        ))}
      </fieldset>
      {heldOut.datasets > 0 && isSmokeEval(heldOut.examples, smoke) && (
        <p className="meta">The attached datasets hold {heldOut.examples} {heldOut.examples === 1 ? 'example' : 'examples'} out, so an eval of this recipe's adapter would be a smoke result (under {smoke}). More data, or a larger held-out share, gives a measurement.</p>
      )}
      <div className="flex flex-wrap items-center gap-3">
        <button onClick={() => save.mutate()} disabled={!dirty || save.isPending} className={btn.secondarySm}>Save</button>
        <input value={model} onChange={(e) => setModel(e.target.value)} aria-label="Model for flesh out" title="The model that writes the spec: auto routes under the training class (local first), or name an endpoint or alias" className={clsx(inputSm, 'w-40 font-mono')} />
        <button onClick={() => flesh.mutate()} disabled={dirty || working || flesh.isPending || !recipe.instructions.trim()} title={dirty ? 'Save your changes first' : undefined} className={btn.primarySm}>
          {working ? 'Fleshing out…' : 'Flesh out'}
        </button>
        {recipe.edited.length > 0 && (
          <label className="flex items-center gap-1 text-xs text-fg-2">
            <input type="checkbox" className="accent-accent" checked={replace} onChange={(e) => setReplace(e.target.checked)} /> replace my edits ({recipe.edited.length})
          </label>
        )}
        <button onClick={() => setShowPrompt(!showPrompt)} className={btn.link}>{showPrompt ? 'hide what the model is asked' : 'what the model is asked'}</button>
      </div>
      {(save.error || flesh.error) && <Callout kind="error">{(save.error ?? flesh.error)!.message}</Callout>}
      {recipe.spec_status === 'failed' && <Callout kind="error">Flesh out failed: {recipe.spec_error}</Callout>}
      {recipe.spec_status === 'ready' && recipe.spec_used_model && (
        <p className="meta">Last draft by {recipe.spec_used_model}{recipe.spec_at ? ` · ${ago(recipe.spec_at)}` : ''}. A model wrote it: read it before you rely on it.</p>
      )}
      {showPrompt && <PromptView />}
      {hasSpec && <SpecEditor recipe={recipe} datasets={datasets} onChange={onChange} />}
      {hasSpec && <TrainFromRecipe recipe={recipe} datasets={datasets} dirty={dirty} />}
    </div>
  )
}

function PromptView() {
  const q = useQuery({ queryKey: ['training', 'spec-prompt'], queryFn: () => api.get<{ prompt: string; schema: unknown }>('/api/training/spec-prompt'), staleTime: Infinity })
  if (!q.data) return <p className="meta">Loading…</p>
  return (
    <div className="space-y-2 border-t border-line pt-2">
      <p className="meta">Sent as the system prompt, with your instructions, the base model and a few sample records of each attached dataset (never the whole file) as the message, under the training task class.</p>
      <pre className="reading whitespace-pre-wrap text-xs">{q.data.prompt}</pre>
      <pre className="whitespace-pre-wrap font-mono text-xs text-fg-2">{JSON.stringify(q.data.schema, null, 2)}</pre>
    </div>
  )
}

function Edited({ on }: { on: boolean }) {
  return on ? <span className="meta ml-2">edited by you</span> : null
}

function SpecEditor({ recipe, datasets, onChange }: { recipe: Recipe; datasets: Dataset[]; onChange: () => void }) {
  const sp = recipe.spec
  const [system, setSystem] = useState(sp.system_prompt ?? '')
  const [rubric, setRubric] = useState(sp.rubric ?? '')
  const [notes, setNotes] = useState(sp.data_notes ?? '')
  const [hyper, setHyper] = useState(sp.hyperparameters ?? { epochs: 2, learning_rate: 2e-4, rank: 16, max_seq_len: 2048 })
  const [maps, setMaps] = useState<SpecMapping[]>(sp.mappings ?? [])
  const is = (k: keyof RecipeSpec) => recipe.edited.includes(k)
  const save = useMutation({
    mutationFn: () => {
      const body: RecipeSpec = {}
      if (system !== (sp.system_prompt ?? '')) body.system_prompt = system
      if (rubric !== (sp.rubric ?? '')) body.rubric = rubric
      if (notes !== (sp.data_notes ?? '')) body.data_notes = notes
      if (JSON.stringify(hyper) !== JSON.stringify(sp.hyperparameters ?? null)) body.hyperparameters = hyper
      if (JSON.stringify(maps) !== JSON.stringify(sp.mappings ?? [])) body.mappings = maps
      return api.put<Recipe>(`/api/training/recipes/${recipe.id}/spec`, body)
    },
    onSuccess: onChange,
  })
  const dirty =
    system !== (sp.system_prompt ?? '') || rubric !== (sp.rubric ?? '') || notes !== (sp.data_notes ?? '') || JSON.stringify(hyper) !== JSON.stringify(sp.hyperparameters ?? null) || JSON.stringify(maps) !== JSON.stringify(sp.mappings ?? [])
  const setMap = (i: number, k: keyof SpecMapping, v: string) => setMaps(maps.map((m, j) => (j === i ? { ...m, [k]: v } : m)))
  const num = (k: 'epochs' | 'learning_rate' | 'rank' | 'max_seq_len', label: string, step?: string) => (
    <label className="flex flex-col gap-1 text-xs text-fg-2">
      {label}
      <input type="number" step={step} value={hyper[k]} onChange={(e) => setHyper({ ...hyper, [k]: Number(e.target.value) })} className={clsx(inputSm, 'tnum')} aria-label={label} />
    </label>
  )
  return (
    <div className="space-y-3 border-t border-line pt-3">
      <div className="space-y-1">
        <span className="section-head border-0">system prompt<Edited on={is('system_prompt')} /></span>
        <textarea value={system} onChange={(e) => setSystem(e.target.value)} rows={7} aria-label="Spec system prompt" className={clsx(inputSm, 'reading')} />
      </div>
      <div className="space-y-1">
        <span className="section-head border-0">eval rubric<Edited on={is('rubric')} /></span>
        <textarea value={rubric} onChange={(e) => setRubric(e.target.value)} rows={4} aria-label="Spec eval rubric" className={clsx(inputSm, 'reading')} />
      </div>
      <div className="space-y-1">
        <span className="section-head border-0">hyperparameters<Edited on={is('hyperparameters')} /></span>
        <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
          {num('epochs', 'epochs')}
          {num('learning_rate', 'learning rate', 'any')}
          {num('rank', 'rank')}
          {num('max_seq_len', 'max sequence length')}
        </div>
      </div>
      <div className="space-y-1">
        <span className="section-head border-0">column mappings<Edited on={is('mappings')} /></span>
        {maps.length === 0 && <p className="meta">None: the attached datasets are used as they are.</p>}
        {maps.map((m, i) => (
          <div key={m.dataset_id} className="space-y-1 rounded-lg border border-line p-2">
            <p className="meta">{datasets.find((d) => d.id === m.dataset_id)?.name ?? m.dataset_id}{m.note ? ` · ${m.note}` : ''}</p>
            <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
              {(['input', 'output', 'system', 'split'] as const).map((k) => (
                <input key={k} value={m[k]} onChange={(e) => setMap(i, k, e.target.value)} placeholder={k} aria-label={`Mapping ${k} for ${datasets.find((d) => d.id === m.dataset_id)?.name ?? m.dataset_id}`} className={clsx(inputSm, 'font-mono')} />
              ))}
            </div>
          </div>
        ))}
      </div>
      <div className="space-y-1">
        <span className="section-head border-0">what the data is missing<Edited on={is('data_notes')} /></span>
        <textarea value={notes} onChange={(e) => setNotes(e.target.value)} rows={3} aria-label="Spec data notes" className={clsx(inputSm, 'reading')} />
      </div>
      <div className="flex items-center gap-3">
        <button onClick={() => save.mutate()} disabled={!dirty || save.isPending} className={btn.primarySm}>Save spec</button>
        {save.error && <Callout kind="error">{save.error.message}</Callout>}
      </div>
    </div>
  )
}

const ADAPTER_NAME = /^[a-z0-9][a-z0-9._-]{1,63}$/

/**
 * Train an adapter from the recipe: its datasets are merged under the spec
 * (the system prompt on every example, the spec's mappings), the usual job
 * runs on the chosen target, and the adapter keeps the system prompt and
 * rubric. The job and the adapter show in the lists below.
 */
function TrainFromRecipe({ recipe, datasets, dirty }: { recipe: Recipe; datasets: Dataset[]; dirty: boolean }) {
  const qc = useQueryClient()
  const jobs = useQuery({ queryKey: ['training', 'jobs'], queryFn: () => api.get<{ jobs: FinetuneJob[]; runner: boolean; targets: FinetuneTarget[] }>('/api/training/jobs') })
  const targets = jobs.data?.targets ?? []
  const [adapter, setAdapter] = useState(recipe.name.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40))
  const [target, setTarget] = useState(recipe.target)
  const ready = recipe.dataset_ids.filter((id) => datasets.find((d) => d.id === id)?.status === 'ready').length
  const start = useMutation({
    mutationFn: () => api.post<FinetuneJob>('/api/training/jobs', { recipe_id: recipe.id, adapter_name: adapter, config: target ? { target } : {} }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['training'] }),
  })
  const why = dirty ? 'Save your changes first.' : !targets.length ? 'No fine-tune runner is configured (see jobs below).' : ready === 0 ? 'Attach a ready dataset first.' : !ADAPTER_NAME.test(adapter) ? 'Adapter name: lowercase letters, digits, dots, dashes and underscores, 2 to 64 characters.' : ''
  return (
    <div className="space-y-2 border-t border-line pt-3">
      <span className="section-head border-0">train</span>
      <div className="flex flex-wrap items-center gap-2">
        <input value={adapter} onChange={(e) => setAdapter(e.target.value)} aria-label="Adapter name" placeholder="adapter name" className={clsx(inputSm, 'w-48 font-mono')} />
        {targets.length > 1 && (
          <select value={target} onChange={(e) => setTarget(e.target.value)} aria-label="Where to train" className={clsx(inputSm, 'w-auto')}>
            <option value="">default target</option>
            {targets.map((t) => <option key={t.id} value={t.id}>{t.label}</option>)}
          </select>
        )}
        <button onClick={() => start.mutate()} disabled={!!why || start.isPending} title={why || undefined} className={btn.primarySm}>Train from this recipe</button>
      </div>
      <p className="meta">{why || `Merges ${ready} ${ready === 1 ? 'dataset' : 'datasets'} under the spec above, with its system prompt on every example, then trains. The adapter is judged by the rubric against its base given the same prompt.`}</p>
      {start.error && <Callout kind="error">{start.error.message}</Callout>}
      {start.isSuccess && <p className="meta">Job queued: it shows under jobs below, and the merged dataset under datasets.</p>}
    </div>
  )
}
