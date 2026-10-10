import { Fragment, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '../api'
import { SectionHead, PageTitle, ListGroup, Row, Callout, btn, inputSm } from '../components/ui'

type Invite = { id: string; email: string; role: string; created_at: string; expires_at: string; used_at: string | null }
type Usage = {
  since: string
  total_usd: number
  by_endpoint: { endpoint_id: string | null; calls: number; input_tokens: number; output_tokens: number; cost_usd: number; avg_latency_ms: number | null }[]
  by_user: { user_id: string | null; calls: number; cost_usd: number }[]
}
type MediaCaps = { engine: string; image?: boolean; image_edit?: boolean; video?: boolean; image_to_video?: boolean; upscale?: boolean; sizes?: string[]; max_images?: number; max_seconds?: number; seconds?: number[] }
type Caps = { context_window: number; max_output: number; tools: boolean; vision: boolean; reasoning: boolean; prompt_cache: boolean; json_mode?: boolean; embeddings?: boolean; max_concurrency?: number; media?: MediaCaps }
type Pricing = { input_per_m: number; output_per_m: number; cache_read_per_m?: number; cache_write_per_m?: number; per_image?: number; per_second?: number }
type EndpointRow = {
  id: string; display_name: string; provider_id: string; model_name: string; enabled: boolean; local: boolean
  capabilities: Caps
  pricing: Pricing
  throughput_class: string; latency_class: string
  health: { status: string; p50_latency_ms: number; error: string }
  extra_body?: { provider?: { order?: string[]; only?: string[] } } & Record<string, unknown>
}
/** A provider row as Admin sees it: the key is named, never shown. */
type ProviderRow = {
  id: string; kind: string; name: string; base_url: string; base_url_env: string; api_key_env: string
  headers: Record<string, string>; configured: boolean; reason?: string
}
type EngineInfo = { id: string; name: string; image: boolean; image_edit: boolean; video: boolean; image_to_video: boolean; upscale: boolean; sizes: string[]; provider_kind: string; note: string }
type Endpoints = {
  providers: { id: string; kind: string; name: string; base_url: string }[]
  all_providers?: ProviderRow[]
  engines?: EngineInfo[]
  endpoints: EndpointRow[]
  /** Decode speed the gateway has measured since the server started; absent for an endpoint never used. */
  throughput?: Record<string, { tokens_per_sec: number; ttft_ms: number; samples: number; in_flight: number; queued?: number }>
}
type CatalogModel = {
  id: string; name: string; description?: string; added: boolean; endpoint_id: string
  capabilities: Caps
  pricing: { input_per_m: number; output_per_m: number }
}
type CatalogRoute = {
  provider: string; tag: string; added: boolean; endpoint_id: string; quantization?: string; uptime_30m?: number; status?: string
  capabilities: Caps
  pricing: { input_per_m: number; output_per_m: number }
}

/** Upstream pinned in an endpoint's extra_body, e.g. "cerebras/fp16", or null. */
function routeOf(e: { extra_body?: { provider?: { order?: string[]; only?: string[] } } }): string | null {
  const p = e.extra_body?.provider
  return p?.order?.[0] ?? p?.only?.[0] ?? null
}
type Policy = { name: string; yaml: string; priority: number; enabled: boolean; error?: string; unknown_endpoints?: string[] }
type Budget = { id: string; scope: string; scope_id: string | null; period: string; limit_usd: number; on_exceed: string }
type User = { id: string; email: string; display_name: string; role: string; shared_provider_keys?: boolean; disabled_at?: string | null }

export default function Admin() {
  const qc = useQueryClient()
  const [email, setEmail] = useState('')
  const [link, setLink] = useState<string | null>(null)
  const [inviteErr, setInviteErr] = useState<string | null>(null)
  const invites = useQuery({ queryKey: ['invites'], queryFn: () => api.get<Invite[]>('/api/admin/invites') })
  const usage = useQuery({ queryKey: ['usage'], queryFn: () => api.get<Usage>('/api/admin/usage?days=30') })
  const eps = useQuery({ queryKey: ['admin-endpoints'], queryFn: () => api.get<Endpoints>('/api/admin/endpoints') })
  const people = useQuery({ queryKey: ['admin-users'], queryFn: () => api.get<User[]>('/api/admin/users') })
  const grant = useMutation({
    mutationFn: ({ id, shared }: { id: string; shared: boolean }) => api.put(`/api/admin/users/${id}/shared-keys`, { shared }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['admin-users'] }),
  })
  const disable = useMutation({
    mutationFn: ({ id, disabled }: { id: string; disabled: boolean }) => api.put(`/api/admin/users/${id}/disabled`, { disabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['admin-users'] }),
  })

  const invite = useMutation({
    mutationFn: () => api.post<{ link: string }>('/api/admin/invites', { email }),
    onSuccess: (r) => {
      setLink(r.link)
      setEmail('')
      setInviteErr(null)
      qc.invalidateQueries({ queryKey: ['invites'] })
    },
    // The server says why (a known address, a bad one); the input keeps its
    // text so the owner can correct it.
    onError: (e) => {
      setLink(null)
      setInviteErr((e as Error).message.replace(/^auth: /, ''))
    },
  })
  const revokeInvite = useMutation({
    mutationFn: (id: string) => api.del(`/api/admin/invites/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['invites'] }),
    onError: (e) => setInviteErr((e as Error).message.replace(/^auth: /, '')),
  })
  const resendInvite = useMutation({
    mutationFn: (id: string) => api.post<{ link: string }>(`/api/admin/invites/${id}/resend`, {}),
    onSuccess: (r) => {
      setLink(r.link)
      setInviteErr(null)
      qc.invalidateQueries({ queryKey: ['invites'] })
    },
    onError: (e) => setInviteErr((e as Error).message.replace(/^auth: /, '')),
  })
  const toggle = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => api.post(`/api/admin/endpoints/${id}/enabled`, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['admin-endpoints'] }),
  })
  const removeEndpoint = useMutation({
    mutationFn: (id: string) => api.del(`/api/admin/endpoints/${id}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['admin-endpoints'] })
      qc.invalidateQueries({ queryKey: ['models'] })
    },
  })
  const [editing, setEditing] = useState<EndpointRow | 'new' | null>(null)

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-3xl p-6 space-y-10">
        <PageTitle>Admin</PageTitle>

        <section className="space-y-3">
          <SectionHead>invites</SectionHead>
          <div className="flex gap-2">
            <input value={email} onChange={(e) => setEmail(e.target.value)} placeholder="friend@example.com" className={inputSm} />
            <button onClick={() => invite.mutate()} className={btn.primarySm}>Invite</button>
          </div>
          {inviteErr && <Callout kind="error">{inviteErr}</Callout>}
          {link && (
            <p className="text-sm text-fg-2">
              Invite link, also emailed when Resend is configured: <code className="font-mono text-xs break-all select-all">{link}</code>
            </p>
          )}
          <ListGroup empty="No invites yet.">
            {invites.data?.map((i) => (
              <li key={i.id} className="flex items-center justify-between gap-3 px-3 py-2">
                <span className="truncate">{i.email} <span className="text-fg-3">· {i.role}</span></span>
                <span className="flex items-center gap-3">
                  <span className="meta">{i.used_at ? 'accepted' : new Date(i.expires_at) < new Date() ? 'expired' : 'pending'}</span>
                  {!i.used_at && (
                    <>
                      <button onClick={() => resendInvite.mutate(i.id)} aria-label={`Resend invite to ${i.email}`} className={btn.secondarySm}>
                        Resend
                      </button>
                      <button onClick={() => revokeInvite.mutate(i.id)} aria-label={`Revoke invite to ${i.email}`} className={btn.danger}>
                        Revoke
                      </button>
                    </>
                  )}
                </span>
              </li>
            ))}
          </ListGroup>
        </section>

        <section className="space-y-3">
          <SectionHead>people</SectionHead>
          <p className="text-sm text-fg-2">
            Members use hosted models only with their own API key (Settings, provider keys). Tick a person to let them use this server's shared keys too; spend on a shared key counts against their budget.
            Keys shows a person's API keys so you can revoke one; Sign out everywhere ends all their browser sessions (passkeys and keys stay, and they can sign in again at once).
          </p>
          <ListGroup empty="No people yet.">
            {people.data?.filter((u) => u.role !== 'owner').map((u) => (
              <PersonRow key={u.id} user={u} onGrant={(shared) => grant.mutate({ id: u.id, shared })} onDisable={(disabled) => disable.mutate({ id: u.id, disabled })} />
            ))}
          </ListGroup>
        </section>

        <Providers rows={eps.data?.all_providers ?? []} />

        <section className="space-y-3">
          <SectionHead aside={`${eps.data?.endpoints.length ?? 0} configured`}>endpoints</SectionHead>
          {editing && (
            <EndpointForm
              initial={editing === 'new' ? undefined : editing}
              providers={eps.data?.all_providers ?? []}
              engines={eps.data?.engines ?? []}
              onClose={() => setEditing(null)}
            />
          )}
          <ListGroup empty="No endpoints configured.">
            {eps.data?.endpoints.map((e) => (
              <li key={e.id} className="flex items-center justify-between px-3 py-2 gap-3">
                <div className="min-w-0">
                  <div className="truncate">{e.display_name} <span className="font-mono text-xs text-fg-3">{e.id}</span></div>
                  <div className="meta">
                    {e.health.status}
                    {e.health.p50_latency_ms ? ` · ${e.health.p50_latency_ms} ms` : ''}
                    {eps.data?.throughput?.[e.id]?.samples ? ` · ${Math.round(eps.data.throughput[e.id].tokens_per_sec)} tok/s · ${Math.round(eps.data.throughput[e.id].ttft_ms)} ms to first token` : ''}
                    {eps.data?.throughput?.[e.id]?.in_flight ? ` · ${eps.data.throughput[e.id].in_flight}${e.capabilities?.max_concurrency ? ` of ${e.capabilities.max_concurrency}` : ''} busy` : ''}
                    {eps.data?.throughput?.[e.id]?.queued ? ` · ${eps.data.throughput[e.id].queued} waiting` : ''}
                    {e.capabilities?.context_window ? ` · ${Math.round(e.capabilities.context_window / 1000)}k ctx` : ''}
                    {e.local ? ' · local' : e.capabilities?.media ? mediaPrice(e.pricing) : e.pricing ? ` · $${e.pricing.input_per_m}/$${e.pricing.output_per_m} per M` : ''}
                    {e.capabilities?.tools ? ' · tools' : ''}{e.capabilities?.vision ? ' · vision' : ''}
                    {e.capabilities?.media ? ` · ${e.capabilities.media.video ? 'video' : 'image'} model (${e.capabilities.media.engine})` : ''}
                    {routeOf(e) ? ` · via ${routeOf(e)}` : ''}
                    {e.health.error ? ` · ${e.health.error}` : ''}
                  </div>
                </div>
                <div className="flex items-center gap-3 shrink-0">
                  <label className="flex items-center gap-2 text-xs text-fg-2">
                    <input type="checkbox" className="accent-accent" checked={e.enabled} onChange={(ev) => toggle.mutate({ id: e.id, enabled: ev.target.checked })} /> enabled
                  </label>
                  <button onClick={() => setEditing(e)} className="text-xs text-fg-2 hover:text-fg">Edit</button>
                  <button onClick={() => removeEndpoint.mutate(e.id)} className={btn.danger}>Remove</button>
                </div>
              </li>
            ))}
          </ListGroup>
          <div className="flex flex-wrap gap-2">
            {!editing && (
              <button onClick={() => setEditing('new')} className={btn.secondarySm}>
                Add a model
              </button>
            )}
            <AddFromCatalog providers={eps.data?.providers ?? []} />
          </div>
        </section>

        <Rentals />
        <GitHub />
        <Policies />
        <Budgets />

        <section className="space-y-3">
          <SectionHead aside={`$${Number(usage.data?.total_usd ?? 0).toFixed(4)} total`}>usage, last 30 days</SectionHead>
          <table className="w-full text-sm">
            <thead className="text-left">
              <tr className="section-head text-[13px] border-0">
                <th className="py-1 font-normal">endpoint</th><th className="font-normal">calls</th><th className="font-normal">in</th><th className="font-normal">out</th><th className="font-normal">cost</th><th className="font-normal">avg ms</th>
              </tr>
            </thead>
            <tbody className="tnum">
              {usage.data?.by_endpoint.map((r) => (
                <tr key={r.endpoint_id ?? 'none'} className="border-t border-line-soft">
                  <td className="py-1.5 font-mono text-xs">{r.endpoint_id ?? '(no route)'}</td>
                  <td>{r.calls}</td><td>{r.input_tokens}</td><td>{r.output_tokens}</td>
                  <td>${Number(r.cost_usd).toFixed(4)}</td><td>{r.avg_latency_ms ?? ''}</td>
                </tr>
              ))}
              {usage.data?.by_endpoint.length === 0 && (
                <tr><td colSpan={6} className="meta py-3">No calls yet.</td></tr>
              )}
            </tbody>
          </table>
        </section>
      </div>
    </div>
  )
}

function mediaPrice(p?: Pricing): string {
  if (!p) return ''
  const parts: string[] = []
  if (p.per_image) parts.push(`$${p.per_image.toFixed(3)}/image`)
  if (p.per_second) parts.push(`$${p.per_second.toFixed(3)}/s`)
  if (p.input_per_m || p.output_per_m) parts.push(`$${p.input_per_m}/$${p.output_per_m} per M tokens`)
  return parts.length ? ' · ' + parts.join(' · ') : ''
}

type AdminKey = { id: string; name: string; prefix: string; scopes: string[]; default_policy: string; created_at: string; last_used_at: string | null }

/** One member in the people list: the shared-keys box, "Sign out everywhere"
 *  and their API keys (loaded when opened), each with Revoke. */
function PersonRow({ user: u, onGrant, onDisable }: { user: User; onGrant: (shared: boolean) => void; onDisable: (disabled: boolean) => void }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [note, setNote] = useState<string | null>(null)
  const keys = useQuery({ queryKey: ['admin-user-keys', u.id], queryFn: () => api.get<AdminKey[]>(`/api/admin/users/${u.id}/keys`), enabled: open })
  const signOut = useMutation({
    mutationFn: () => api.del(`/api/admin/users/${u.id}/sessions`),
    onSuccess: () => setNote('Signed out everywhere.'),
    onError: (e: Error) => setNote(e.message),
  })
  const revoke = useMutation({
    mutationFn: (id: string) => api.del(`/api/admin/users/${u.id}/keys/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['admin-user-keys', u.id] }),
  })
  return (
    <li className="px-3 py-2 space-y-2">
      <div className="flex items-center justify-between gap-3">
        <span className="truncate">
          {u.email}
          {u.disabled_at && <span className="meta ml-2">disabled</span>}
          {note && <span className="meta"> · {note}</span>}
        </span>
        <div className="flex items-center gap-3 shrink-0">
          <label className="meta flex items-center gap-2">
            <input
              type="checkbox"
              aria-label={`${u.email} may use shared keys`}
              checked={!!u.shared_provider_keys}
              onChange={(e) => onGrant(e.target.checked)}
            />
            may use shared keys
          </label>
          <button onClick={() => setOpen((o) => !o)} aria-expanded={open} aria-label={`${u.email} API keys`} className={btn.secondarySm}>
            {open ? 'Hide keys' : 'Keys'}
          </button>
          <button onClick={() => signOut.mutate()} disabled={signOut.isPending} aria-label={`Sign ${u.email} out everywhere`} className={btn.secondarySm}>
            Sign out everywhere
          </button>
          <button
            onClick={() => onDisable(!u.disabled_at)}
            aria-label={`${u.disabled_at ? 'Enable' : 'Disable'} ${u.email}`}
            className={u.disabled_at ? btn.secondarySm : btn.danger}
          >
            {u.disabled_at ? 'Enable' : 'Disable'}
          </button>
        </div>
      </div>
      {open && (
        <ListGroup empty="No API keys.">
          {keys.data?.map((k) => (
            <Row key={k.id} action={<button onClick={() => revoke.mutate(k.id)} disabled={revoke.isPending} aria-label={`Revoke ${k.name}`} className={btn.danger}>Revoke</button>}>
              {k.name} <span className="font-mono text-xs text-fg-3">{k.prefix}…</span>
              <span className="meta"> · policy {k.default_policy || 'auto'} · {k.scopes.join(', ') || 'no scope'}</span>
              {k.last_used_at && <span className="meta"> · used {new Date(k.last_used_at).toLocaleDateString()}</span>}
            </Row>
          ))}
        </ListGroup>
      )}
    </li>
  )
}

/**
 * Providers are the runners: a base URL (literal, or taken from a
 * variable so one config serves every box) and the name of the variable
 * holding the key. The key itself stays in .env or Infisical.
 */
function Providers({ rows }: { rows: ProviderRow[] }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState<ProviderRow | 'new' | null>(null)
  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/admin/providers/${id}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['admin-endpoints'] })
      qc.invalidateQueries({ queryKey: ['models'] })
    },
  })
  return (
    <section className="space-y-3">
      <SectionHead aside={`${rows.filter((p) => p.configured).length} of ${rows.length} configured`}>providers</SectionHead>
      <ListGroup empty="No providers. Add one, or seed config/endpoints.yaml.">
        {rows.map((p) => (
          <li key={p.id} className="flex items-center justify-between px-3 py-2 gap-3">
            <div className="min-w-0">
              <div className="truncate">
                {p.name} <span className="font-mono text-xs text-fg-3">{p.id}</span>
              </div>
              <div className="meta truncate">
                {p.kind} · {p.base_url_env ? `url from ${p.base_url_env}` : p.base_url}
                {p.api_key_env ? ` · key in ${p.api_key_env}` : ' · no key'}
                {p.configured ? '' : ` · not configured: ${p.reason}`}
              </div>
            </div>
            <div className="flex items-center gap-3 shrink-0">
              <button onClick={() => setEditing(p)} className="text-xs text-fg-2 hover:text-fg">Edit</button>
              <button
                onClick={() => {
                  if (confirm(`Remove ${p.id} and every endpoint on it?`)) remove.mutate(p.id)
                }}
                className={btn.danger}
              >
                Remove
              </button>
            </div>
          </li>
        ))}
      </ListGroup>
      {editing ? (
        <ProviderForm initial={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)} />
      ) : (
        <button onClick={() => setEditing('new')} className={btn.secondarySm}>
          Add a provider
        </button>
      )}
      <p className="meta">A provider also in config/endpoints.yaml takes the file's values again at the next boot; change the file for those.</p>
    </section>
  )
}

function ProviderForm({ initial, onClose }: { initial?: ProviderRow; onClose: () => void }) {
  const qc = useQueryClient()
  const [id, setID] = useState(initial?.id ?? '')
  const [name, setName] = useState(initial?.name ?? '')
  const [kind, setKind] = useState(initial?.kind ?? 'openai_compat')
  const [urlMode, setURLMode] = useState<'literal' | 'env'>(initial?.base_url_env ? 'env' : 'literal')
  const [baseURL, setBaseURL] = useState(initial?.base_url ?? '')
  const [baseURLEnv, setBaseURLEnv] = useState(initial?.base_url_env ?? '')
  const [keyEnv, setKeyEnv] = useState(initial?.api_key_env ?? '')
  const [headers, setHeaders] = useState(JSON.stringify(initial?.headers ?? {}))
  const [err, setErr] = useState('')
  const save = useMutation({
    mutationFn: () => {
      let h: Record<string, string> = {}
      try {
        h = headers.trim() ? JSON.parse(headers) : {}
      } catch {
        throw new Error('Headers must be a JSON object of strings.')
      }
      return api.put('/api/admin/providers', {
        id, name, kind,
        base_url: urlMode === 'literal' ? baseURL : '',
        base_url_env: urlMode === 'env' ? baseURLEnv : '',
        api_key_env: keyEnv,
        headers: h,
      })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['admin-endpoints'] })
      qc.invalidateQueries({ queryKey: ['models'] })
      onClose()
    },
    onError: (e) => setErr((e as Error).message),
  })
  return (
    <form
      className="space-y-2 rounded-lg border border-line p-3 text-sm"
      onSubmit={(e) => {
        e.preventDefault()
        save.mutate()
      }}
    >
      <div className="grid grid-cols-2 gap-2">
        <input value={id} onChange={(e) => setID(e.target.value)} placeholder="id, e.g. fal or comfy-spark" className={`${inputSm} font-mono`} disabled={!!initial} required />
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="display name" className={inputSm} />
        <select value={kind} onChange={(e) => setKind(e.target.value)} className={inputSm} aria-label="Kind">
          <option value="openai_compat">openai_compat (OpenAI-shaped API)</option>
          <option value="anthropic">anthropic</option>
        </select>
        <select value={urlMode} onChange={(e) => setURLMode(e.target.value as 'literal' | 'env')} className={inputSm} aria-label="Base URL source">
          <option value="literal">base URL in the row</option>
          <option value="env">base URL from a variable</option>
        </select>
        {urlMode === 'literal' ? (
          <input value={baseURL} onChange={(e) => setBaseURL(e.target.value)} placeholder="https://api.example.com/v1" className={`${inputSm} font-mono col-span-2`} />
        ) : (
          <input value={baseURLEnv} onChange={(e) => setBaseURLEnv(e.target.value)} placeholder="COMFY_URL" className={`${inputSm} font-mono col-span-2`} />
        )}
        <input value={keyEnv} onChange={(e) => setKeyEnv(e.target.value)} placeholder="API key variable, e.g. FAL_KEY (empty for a local server)" className={`${inputSm} font-mono`} />
        <input value={headers} onChange={(e) => setHeaders(e.target.value)} placeholder='extra headers, JSON' className={`${inputSm} font-mono`} />
      </div>
      <p className="meta">The key value is read from that variable on the box (.env or Infisical), never entered here. A provider whose key or URL variable is unset is skipped until it is.</p>
      {err && <Callout kind="error">{err}</Callout>}
      <div className="flex gap-2">
        <button type="submit" disabled={save.isPending || !id} className={btn.primarySm}>
          {initial ? 'Save' : 'Add'}
        </button>
        <button type="button" onClick={onClose} className={btn.secondarySm}>
          Cancel
        </button>
      </div>
    </form>
  )
}

/**
 * One form for every kind of model: a text model declares its context and
 * what it can do; an image or video model declares its engine (the
 * adapter in internal/media), what it makes, sizes, limits and the price
 * per image or second.
 */
function EndpointForm({ initial, providers, engines, onClose }: { initial?: EndpointRow; providers: ProviderRow[]; engines: EngineInfo[]; onClose: () => void }) {
  const qc = useQueryClient()
  const media0 = initial?.capabilities?.media
  const [kind, setKind] = useState<'text' | 'media'>(media0 ? 'media' : 'text')
  const [provider, setProvider] = useState(initial?.provider_id ?? providers[0]?.id ?? '')
  const [model, setModel] = useState(initial?.model_name ?? '')
  const [display, setDisplay] = useState(initial?.display_name ?? '')
  const [local, setLocal] = useState(initial?.local ?? false)
  const [enabled, setEnabled] = useState(initial?.enabled ?? true)
  const [extra, setExtra] = useState(initial?.extra_body && Object.keys(initial.extra_body).length ? JSON.stringify(initial.extra_body) : '')
  // text
  const c = initial?.capabilities
  const [ctx, setCtx] = useState(c?.context_window ?? 128000)
  const [maxOut, setMaxOut] = useState(c?.max_output ?? 8192)
  const [tools, setTools] = useState(c?.tools ?? true)
  const [vision, setVision] = useState(c?.vision ?? false)
  const [jsonMode, setJSONMode] = useState(c?.json_mode ?? true)
  const [reasoning, setReasoning] = useState(c?.reasoning ?? false)
  const [cache, setCache] = useState(c?.prompt_cache ?? false)
  const [embeddings, setEmbeddings] = useState(c?.embeddings ?? false)
  const [slots, setSlots] = useState(c?.max_concurrency ?? 0)
  const [inPerM, setInPerM] = useState(initial?.pricing?.input_per_m ?? 0)
  const [outPerM, setOutPerM] = useState(initial?.pricing?.output_per_m ?? 0)
  // media
  const [engine, setEngine] = useState(media0?.engine ?? engines[0]?.id ?? '')
  const eng = engines.find((e) => e.id === engine)
  const [image, setImage] = useState(media0?.image ?? true)
  const [imageEdit, setImageEdit] = useState(media0?.image_edit ?? false)
  const [video, setVideo] = useState(media0?.video ?? false)
  const [imageToVideo, setImageToVideo] = useState(media0?.image_to_video ?? false)
  const [upscale, setUpscale] = useState(media0?.upscale ?? false)
  const [sizes, setSizes] = useState((media0?.sizes ?? eng?.sizes ?? []).join(', '))
  const [maxImages, setMaxImages] = useState(media0?.max_images ?? 4)
  const [maxSeconds, setMaxSeconds] = useState(media0?.max_seconds ?? 0)
  const [seconds, setSeconds] = useState((media0?.seconds ?? []).join(', '))
  const [perImage, setPerImage] = useState(initial?.pricing?.per_image ?? 0)
  const [perSecond, setPerSecond] = useState(initial?.pricing?.per_second ?? 0)
  const [err, setErr] = useState('')

  const save = useMutation({
    mutationFn: () => {
      let extraBody: Record<string, unknown> | undefined
      if (extra.trim()) {
        try {
          extraBody = JSON.parse(extra)
        } catch {
          throw new Error('extra_body must be a JSON object.')
        }
      }
      const capabilities: Caps =
        kind === 'media'
          ? {
              context_window: 0, max_output: 0, tools: false, vision: false, reasoning: false, prompt_cache: false,
              media: {
                engine, image, image_edit: imageEdit, video, image_to_video: imageToVideo, upscale,
                sizes: sizes.split(',').map((s) => s.trim()).filter(Boolean),
                max_images: maxImages, max_seconds: maxSeconds,
                seconds: seconds.split(',').map((s) => Number(s.trim())).filter((n) => Number.isInteger(n) && n > 0),
              },
            }
          : { context_window: ctx, max_output: maxOut, tools, vision, json_mode: jsonMode, reasoning, prompt_cache: cache, embeddings, max_concurrency: slots || undefined }
      const pricing: Pricing = kind === 'media' ? { input_per_m: inPerM, output_per_m: outPerM, per_image: perImage, per_second: perSecond } : { input_per_m: inPerM, output_per_m: outPerM }
      return api.put('/api/admin/endpoints', {
        id: initial?.id, provider_id: provider, model_name: model, display_name: display || model,
        capabilities, pricing, local, enabled, extra_body: extraBody,
        throughput_class: initial?.throughput_class || (kind === 'media' ? 'medium' : 'high'),
        latency_class: initial?.latency_class || (kind === 'media' ? 'slow' : 'normal'),
      })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['admin-endpoints'] })
      qc.invalidateQueries({ queryKey: ['models'] })
      onClose()
    },
    onError: (e) => setErr((e as Error).message),
  })
  const num = (set: (n: number) => void) => (e: React.ChangeEvent<HTMLInputElement>) => set(Number(e.target.value))
  const check = (label: string, v: boolean, set: (b: boolean) => void) => (
    <label className="flex items-center gap-1.5 text-xs text-fg-2">
      <input type="checkbox" className="accent-accent" checked={v} onChange={(e) => set(e.target.checked)} /> {label}
    </label>
  )
  return (
    <form
      className="space-y-3 rounded-lg border border-line p-3 text-sm"
      onSubmit={(e) => {
        e.preventDefault()
        save.mutate()
      }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <select value={kind} onChange={(e) => setKind(e.target.value as 'text' | 'media')} className={`${inputSm} w-auto`} aria-label="Model kind" disabled={!!initial}>
          <option value="text">text model</option>
          <option value="media">image or video model</option>
        </select>
        <select value={provider} onChange={(e) => setProvider(e.target.value)} className={`${inputSm} w-auto max-w-[14rem]`} aria-label="Provider" disabled={!!initial}>
          {providers.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}{p.configured ? '' : ' (not configured)'}
            </option>
          ))}
        </select>
        <input value={model} onChange={(e) => setModel(e.target.value)} placeholder="model name sent upstream" className={`${inputSm} font-mono w-auto flex-1 min-w-40`} required disabled={!!initial} />
        <input value={display} onChange={(e) => setDisplay(e.target.value)} placeholder="display name" className={`${inputSm} w-auto flex-1 min-w-32`} />
      </div>
      {kind === 'text' ? (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <label className="text-xs text-fg-2 flex items-center gap-1">context <input type="number" value={ctx} onChange={num(setCtx)} className={`${inputSm} w-28 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">max out <input type="number" value={maxOut} onChange={num(setMaxOut)} className={`${inputSm} w-24 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">$/M in <input type="number" step="0.01" value={inPerM} onChange={num(setInPerM)} className={`${inputSm} w-24 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">$/M out <input type="number" step="0.01" value={outPerM} onChange={num(setOutPerM)} className={`${inputSm} w-24 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1" title="Requests the server handles at once at full speed (llama-server --parallel). 0 means unknown.">slots <input type="number" min={0} value={slots} onChange={num(setSlots)} className={`${inputSm} w-16 tnum`} /></label>
          </div>
          <div className="flex flex-wrap gap-3">
            {check('tools', tools, setTools)}{check('vision', vision, setVision)}{check('json mode', jsonMode, setJSONMode)}{check('reasoning', reasoning, setReasoning)}{check('prompt cache', cache, setCache)}{check('embeddings', embeddings, setEmbeddings)}
          </div>
        </>
      ) : (
        <>
          {engines.length === 0 && <Callout kind="error">This build has no media engine; image and video endpoints cannot run.</Callout>}
          <div className="flex flex-wrap items-center gap-2">
            <select value={engine} onChange={(e) => { setEngine(e.target.value); const ne = engines.find((x) => x.id === e.target.value); if (ne && !media0) setSizes(ne.sizes.join(', ')) }} className={`${inputSm} w-auto`} aria-label="Engine">
              {engines.map((e) => (
                <option key={e.id} value={e.id}>
                  {e.name}
                </option>
              ))}
            </select>
            <input value={sizes} onChange={(e) => setSizes(e.target.value)} placeholder="sizes, e.g. 1024x1024, 1536x1024" className={`${inputSm} font-mono flex-1 min-w-48`} />
          </div>
          {eng?.note && <p className="meta">{eng.note}</p>}
          <div className="flex flex-wrap gap-3">
            {check('image', image, setImage)}{check('image edit', imageEdit, setImageEdit)}{check('video', video, setVideo)}{check('image to video', imageToVideo, setImageToVideo)}{check('upscale', upscale, setUpscale)}
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <label className="text-xs text-fg-2 flex items-center gap-1">max images <input type="number" min={1} max={4} value={maxImages} onChange={num(setMaxImages)} className={`${inputSm} w-20 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">max seconds <input type="number" min={0} value={maxSeconds} onChange={num(setMaxSeconds)} className={`${inputSm} w-20 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">lengths <input value={seconds} onChange={(e) => setSeconds(e.target.value)} placeholder="4, 8, 12" title="Video lengths the model accepts, in seconds; the first is the default. Empty: any length up to max seconds." className={`${inputSm} w-24 font-mono`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">$/image <input type="number" step="0.001" value={perImage} onChange={num(setPerImage)} className={`${inputSm} w-24 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">$/second <input type="number" step="0.001" value={perSecond} onChange={num(setPerSecond)} className={`${inputSm} w-24 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">$/M tokens in <input type="number" step="0.01" value={inPerM} onChange={num(setInPerM)} className={`${inputSm} w-20 tnum`} /></label>
            <label className="text-xs text-fg-2 flex items-center gap-1">out <input type="number" step="0.01" value={outPerM} onChange={num(setOutPerM)} className={`${inputSm} w-20 tnum`} /></label>
          </div>
        </>
      )}
      <div className="flex flex-wrap items-center gap-3">
        {check('local (free, preferred on budget downgrade)', local, setLocal)}
        {check('enabled', enabled, setEnabled)}
        <input value={extra} onChange={(e) => setExtra(e.target.value)} placeholder="extra_body JSON, e.g. {&quot;dimensions&quot;:768}" className={`${inputSm} font-mono flex-1 min-w-48`} />
      </div>
      {err && <Callout kind="error">{err}</Callout>}
      <div className="flex gap-2">
        <button type="submit" disabled={save.isPending || !model || !provider} className={btn.primarySm}>
          {initial ? 'Save' : 'Add'}
        </button>
        <button type="button" onClick={onClose} className={btn.secondarySm}>
          Cancel
        </button>
      </div>
    </form>
  )
}

type RentalTemplate = {
  name: string; kind: 'inference' | 'trainer'; provider: string; description: string; gpu: string; gpu_count: number; model: string
  idle_timeout: number; max_hours: number; hourly_usd: number
  endpoint: { id: string; display_name: string }
}
type RentalInstance = {
  id: string; provider: string; template: string; gpu: string; provider_instance_id: string | null; endpoint_id: string; base_url: string | null
  hourly_usd: number; status: 'provisioning' | 'warming' | 'ready' | 'stopping' | 'stopped' | 'failed'
  started_at: string; ready_at: string | null; last_request_at: string | null; stopped_at: string | null
  hours_used: number; stop_reason: string | null; error: string | null; estimated_usd: number
}
type Rentals = {
  templates: RentalTemplate[]
  instances: RentalInstance[]
  providers: Record<string, boolean>
  caps: { daily_cap_hours: number; used_today_hours: number; disabled: boolean; key_set: boolean }
}
const RENTAL_OPEN = new Set(['provisioning', 'warming', 'ready', 'stopping'])

/**
 * Rented GPUs (PLAN M9): start a template, watch it warm up, stop it. The
 * worker reconciles every minute; the list polls while a machine is open.
 */
function Rentals() {
  const qc = useQueryClient()
  const q = useQuery({
    queryKey: ['rentals'],
    queryFn: () => api.get<Rentals>('/api/admin/rentals'),
    retry: false,
    refetchInterval: (x) => (x.state.data?.instances.some((i) => RENTAL_OPEN.has(i.status)) ? 10_000 : false),
  })
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['rentals'] })
    qc.invalidateQueries({ queryKey: ['admin-endpoints'] })
    qc.invalidateQueries({ queryKey: ['models'] })
  }
  const start = useMutation({ mutationFn: (template: string) => api.post('/api/admin/rentals', { template }), onSuccess: refresh })
  const stop = useMutation({ mutationFn: (id: string) => api.post(`/api/admin/rentals/${id}/stop`, {}), onSuccess: refresh })
  const stopAll = useMutation({ mutationFn: () => api.post('/api/admin/rentals/stop-all', {}), onSuccess: refresh })
  if (q.error) {
    return (
      <section className="space-y-3">
        <SectionHead>rented gpus</SectionHead>
        <p className="meta">{(q.error as Error).message}</p>
      </section>
    )
  }
  const d = q.data
  const open = d?.instances.filter((i) => RENTAL_OPEN.has(i.status)) ?? []
  const past = d?.instances.filter((i) => !RENTAL_OPEN.has(i.status)).slice(0, 8) ?? []
  const err = (start.error ?? stop.error ?? stopAll.error) as Error | null
  return (
    <section className="space-y-3">
      <SectionHead aside={d ? `${d.caps.used_today_hours.toFixed(1)} of ${d.caps.daily_cap_hours || '∞'} h today` : undefined}>rented gpus</SectionHead>
      {d && !d.caps.key_set && <Callout kind="note">Set WS_RENTAL_API_KEY on the box (the key rented servers will require) before starting a machine.</Callout>}
      {d && Object.keys(d.providers).length === 0 && <p className="meta">No rental provider is configured. Set RUNPOD_API_KEY to enable RunPod.</p>}
      {d?.caps.disabled && <Callout kind="error">Rentals are disabled by WS_RENTAL_DISABLED; open machines stop at the next reconcile.</Callout>}
      {err && <Callout kind="error">{err.message}</Callout>}
      <ListGroup empty="No templates in infra/rental/templates.">
        {d?.templates.map((t) => {
          const running = open.find((i) => i.template === t.name)
          return (
            <li key={t.name} className="flex items-center justify-between gap-3 px-3 py-2">
              <div className="min-w-0">
                <div className="truncate">
                  {t.endpoint.display_name} <span className="font-mono text-xs text-fg-3">{t.name}</span>
                </div>
                <div className="meta truncate">
                  {t.gpu}{t.gpu_count > 1 ? ` ×${t.gpu_count}` : ''} on {t.provider} · about ${t.hourly_usd.toFixed(2)}/h · idle stop {Math.round(t.idle_timeout / 6e10)} min · max {t.max_hours} h
                  {t.description ? ` · ${t.description}` : ''}
                </div>
              </div>
              {running ? (
                <span className="meta shrink-0">{running.status}</span>
              ) : t.kind === 'trainer' ? (
                <span className="meta shrink-0" title="Set WS_FINETUNE_TEMPLATE to this template; a fine-tune job targeting a rented GPU starts and stops it">started by fine-tune jobs</span>
              ) : (
                <button onClick={() => start.mutate(t.name)} disabled={start.isPending || !d.providers[t.provider] || !d.caps.key_set || d.caps.disabled} className={btn.primarySm}>
                  Start · ${t.hourly_usd.toFixed(2)}/h
                </button>
              )}
            </li>
          )
        })}
      </ListGroup>
      {open.length > 0 && (
        <>
          <ListGroup>
            {open.map((i) => (
              <li key={i.id} className="flex items-center justify-between gap-3 px-3 py-2">
                <div className="min-w-0">
                  <div className="truncate">
                    {i.template} <span className="font-mono text-xs text-fg-3">{i.endpoint_id}</span>
                  </div>
                  <div className="meta truncate">
                    {i.status === 'ready' ? 'ready and routable' : i.status === 'warming' ? 'machine up, model loading…' : i.status === 'provisioning' ? 'asking the provider…' : i.status}
                    {` · ${i.hours_used.toFixed(2)} h · $${i.estimated_usd.toFixed(2)} so far at $${i.hourly_usd.toFixed(2)}/h`}
                    {i.last_request_at ? ` · last request ${agoShort(i.last_request_at)}` : ''}
                    {i.error ? ` · ${i.error}` : ''}
                  </div>
                </div>
                <button onClick={() => stop.mutate(i.id)} disabled={stop.isPending || i.status === 'stopping'} className={btn.secondarySm}>
                  Stop
                </button>
              </li>
            ))}
          </ListGroup>
          <button
            onClick={() => {
              if (confirm('Stop every rented machine now?')) stopAll.mutate()
            }}
            className={btn.danger}
          >
            stop all
          </button>
        </>
      )}
      {past.length > 0 && (
        <details className="text-sm">
          <summary className="cursor-pointer meta">past machines</summary>
          <ul className="mt-2 divide-y divide-line rounded-lg border border-line">
            {past.map((i) => (
              <li key={i.id} className="px-3 py-2 meta truncate">
                {i.template} · {i.status}{i.stop_reason ? ` (${i.stop_reason})` : ''} · {i.hours_used.toFixed(2)} h · ${i.estimated_usd.toFixed(2)} · {agoShort(i.started_at)}
                {i.error ? ` · ${i.error}` : ''}
              </li>
            ))}
          </ul>
        </details>
      )}
      <p className="meta">Each whole hour and the final fraction land in the ledger under task class rental. Machines stop on idle, max hours, the daily cap, or Stop.</p>
    </section>
  )
}

function agoShort(iso: string): string {
  const m = Math.floor((Date.now() - new Date(iso).getTime()) / 60_000)
  if (!Number.isFinite(m) || m < 0) return 'now'
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  return h < 48 ? `${h}h ago` : `${Math.floor(h / 24)}d ago`
}

/** Browse a provider's catalog (OpenRouter today) and add models as endpoints. */
function AddFromCatalog({ providers }: { providers: { id: string; base_url: string; name: string }[] }) {
  const qc = useQueryClient()
  const catalogProviders = providers.filter((p) => p.base_url.includes('openrouter.ai'))
  const [provider, setProvider] = useState<string>('')
  const [q, setQ] = useState('')
  const [open, setOpen] = useState(false)
  const [routesFor, setRoutesFor] = useState<string | null>(null)
  const pid = provider || catalogProviders[0]?.id || ''
  const catalog = useQuery({
    queryKey: ['catalog', pid, q],
    queryFn: () => api.get<{ count: number; models: CatalogModel[] }>(`/api/admin/providers/${pid}/catalog?q=${encodeURIComponent(q)}`),
    enabled: open && pid !== '',
    staleTime: 60_000,
  })
  const add = useMutation({
    mutationFn: (m: CatalogModel) =>
      api.put('/api/admin/endpoints', {
        id: m.endpoint_id, provider_id: pid, model_name: m.id, display_name: m.name,
        capabilities: m.capabilities, pricing: m.pricing, throughput_class: 'high', latency_class: 'normal',
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['admin-endpoints'] })
      qc.invalidateQueries({ queryKey: ['models'] })
      qc.invalidateQueries({ queryKey: ['catalog'] })
    },
  })

  if (catalogProviders.length === 0) {
    return <p className="meta">Add an OpenRouter key to browse and add hosted models here.</p>
  }
  if (!open) {
    return <button onClick={() => setOpen(true)} className={btn.secondarySm}>Add models from OpenRouter</button>
  }
  return (
    <div className="space-y-2 rounded-lg border border-line p-3">
      <div className="flex gap-2 items-center">
        {catalogProviders.length > 1 && (
          <select value={pid} onChange={(e) => setProvider(e.target.value)} className={`${inputSm} max-w-[12rem]`}>
            {catalogProviders.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
        )}
        <input autoFocus value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search models, e.g. claude, qwen, gemini…" className={inputSm} />
        <button onClick={() => setOpen(false)} className={btn.secondarySm}>Close</button>
      </div>
      {catalog.isLoading && <p className="meta">Loading catalog…</p>}
      {catalog.error && <Callout kind="error">{(catalog.error as Error).message}</Callout>}
      {catalog.data && (
        <>
          <p className="meta">{catalog.data.count} models on {pid}{q ? `, ${catalog.data.models.length} match` : ', showing the first 200'}.</p>
          <ul className="divide-y divide-line rounded-lg border border-line text-sm max-h-96 overflow-y-auto">
            {catalog.data.models.map((m) => (
              <Fragment key={m.id}>
                <li className="flex items-center justify-between gap-3 px-3 py-2">
                  <div className="min-w-0">
                    <div className="truncate">{m.name} <span className="font-mono text-xs text-fg-3">{m.id}</span></div>
                    <div className="meta">
                      {m.capabilities.context_window ? `${Math.round(m.capabilities.context_window / 1000)}k ctx` : 'ctx unknown'}
                      {` · $${m.pricing.input_per_m.toFixed(2)} in / $${m.pricing.output_per_m.toFixed(2)} out per M`}
                      {m.capabilities.tools ? ' · tools' : ''}{m.capabilities.vision ? ' · vision' : ''}{m.capabilities.reasoning ? ' · reasoning' : ''}
                    </div>
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    <button onClick={() => setRoutesFor(routesFor === m.id ? null : m.id)} className={btn.secondarySm}>
                      {routesFor === m.id ? 'Hide routes' : 'Routes'}
                    </button>
                    {m.added ? (
                      <span className="text-xs text-fg-3">added</span>
                    ) : (
                      <button onClick={() => add.mutate(m)} disabled={add.isPending} className={btn.secondarySm}>Add</button>
                    )}
                  </div>
                </li>
                {routesFor === m.id && (
                  <li className="px-3 py-2">
                    <Routes pid={pid} model={m} />
                  </li>
                )}
              </Fragment>
            ))}
            {catalog.data.models.length === 0 && <li className="px-3 py-3 meta">Nothing matches.</li>}
          </ul>
        </>
      )}
    </div>
  )
}

function Policies() {
  const qc = useQueryClient()
  const policies = useQuery({ queryKey: ['policies'], queryFn: () => api.get<Policy[]>('/api/admin/policies') })
  const [editing, setEditing] = useState<Policy | null>(null)
  const [err, setErr] = useState<string | null>(null)

  const save = useMutation({
    mutationFn: (p: Policy) => api.put<Policy>(`/api/admin/policies/${encodeURIComponent(p.name)}`, { yaml: p.yaml, priority: p.priority, enabled: p.enabled }),
    onSuccess: () => {
      setEditing(null)
      setErr(null)
      qc.invalidateQueries({ queryKey: ['policies'] })
      qc.invalidateQueries({ queryKey: ['models'] })
    },
    onError: (e) => setErr(e.message),
  })
  const remove = useMutation({
    mutationFn: (name: string) => api.del(`/api/admin/policies/${encodeURIComponent(name)}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['policies'] }),
  })

  return (
    <section className="space-y-3">
      <SectionHead aside="lower priority wins">routing policies</SectionHead>
      <p className="text-sm text-fg-2">Rules match by task class and yield an ordered preference list; the gateway fails over down the list. Aliases become model names. Changes apply immediately.</p>
      <ListGroup empty="No policies.">
        {policies.data?.map((p) => (
          <li key={p.name} className="px-3 py-2 space-y-1">
            <div className="flex items-center justify-between gap-3">
              <span className="min-w-0 truncate">
                <span className="font-mono text-xs">{p.name}</span>
                <span className="meta ml-2">priority {p.priority}{p.enabled ? '' : ' · disabled'}</span>
              </span>
              <span className="flex gap-3 shrink-0">
                <button onClick={() => setEditing({ ...p })} className="text-xs text-accent">Edit</button>
                <button onClick={() => remove.mutate(p.name)} className={btn.danger}>Delete</button>
              </span>
            </div>
            {p.error && <div className="text-xs text-danger">{p.error}</div>}
            {p.unknown_endpoints && p.unknown_endpoints.length > 0 && (
              <div className="text-xs text-fg-3">unknown endpoints on this box: {p.unknown_endpoints.join(', ')}</div>
            )}
          </li>
        ))}
      </ListGroup>
      {editing ? (
        <div className="space-y-2 rounded-lg border border-line p-3">
          <div className="flex gap-2 items-center">
            <input value={editing.name} onChange={(e) => setEditing({ ...editing, name: e.target.value })} placeholder="name" className={`${inputSm} font-mono max-w-[14rem]`} />
            <input type="number" value={editing.priority} onChange={(e) => setEditing({ ...editing, priority: Number(e.target.value) })} className={`${inputSm} max-w-[6rem] tnum`} />
            <label className="text-xs text-fg-2 flex items-center gap-1"><input type="checkbox" className="accent-accent" checked={editing.enabled} onChange={(e) => setEditing({ ...editing, enabled: e.target.checked })} /> enabled</label>
          </div>
          <textarea
            value={editing.yaml}
            onChange={(e) => setEditing({ ...editing, yaml: e.target.value })}
            rows={14}
            spellCheck={false}
            className="w-full rounded-lg border border-line bg-bg-2 px-3 py-2 font-mono text-xs leading-relaxed outline-none focus:border-accent"
          />
          {err && <Callout kind="error">{err}</Callout>}
          <div className="flex gap-2">
            <button onClick={() => save.mutate(editing)} className={btn.primarySm}>Save</button>
            <button onClick={() => { setEditing(null); setErr(null) }} className={btn.secondarySm}>Cancel</button>
          </div>
        </div>
      ) : (
        <button onClick={() => setEditing({ name: 'custom', yaml: 'name: custom\npriority: 50\nrules:\n  - match: { task_class: [chat] }\n    prefer: []\n', priority: 50, enabled: true })} className={btn.secondarySm}>
          New policy
        </button>
      )}
    </section>
  )
}

function Budgets() {
  const qc = useQueryClient()
  const budgets = useQuery({ queryKey: ['budgets'], queryFn: () => api.get<Budget[]>('/api/admin/budgets') })
  const users = useQuery({ queryKey: ['admin-users'], queryFn: () => api.get<User[]>('/api/admin/users') })
  const [form, setForm] = useState({ scope: 'global', scope_id: '', period: 'month', limit_usd: 20, on_exceed: 'downgrade' })
  const [err, setErr] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => api.put('/api/admin/budgets', form),
    onSuccess: () => { setErr(null); qc.invalidateQueries({ queryKey: ['budgets'] }) },
    onError: (e) => setErr(e.message),
  })
  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/admin/budgets/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['budgets'] }),
  })
  const userName = (id: string | null) => users.data?.find((u) => u.id === id)?.email ?? id ?? ''

  return (
    <section className="space-y-3">
      <SectionHead>budgets</SectionHead>
      <p className="text-sm text-fg-2">Rolling windows. When a budget is spent, requests either downgrade to local models or are blocked. Local endpoints cost nothing.</p>
      <ListGroup empty="No budgets. Spending is unlimited.">
        {budgets.data?.map((b) => (
          <li key={b.id} className="flex items-center justify-between gap-3 px-3 py-2">
            <span className="min-w-0 truncate">
              {b.scope}{b.scope_id ? <span className="text-fg-3"> · {userName(b.scope_id)}</span> : ''} <span className="meta">· ${Number(b.limit_usd).toFixed(2)} per {b.period} · {b.on_exceed}</span>
            </span>
            <button onClick={() => remove.mutate(b.id)} className={btn.danger}>Remove</button>
          </li>
        ))}
      </ListGroup>
      <div className="flex flex-wrap gap-2 items-center">
        <select value={form.scope} onChange={(e) => setForm({ ...form, scope: e.target.value, scope_id: '' })} className={`${inputSm} max-w-[8rem]`}>
          <option value="global">global</option><option value="user">user</option><option value="api_key">api key</option>
        </select>
        {form.scope === 'user' && (
          <select value={form.scope_id} onChange={(e) => setForm({ ...form, scope_id: e.target.value })} className={`${inputSm} max-w-[14rem]`}>
            <option value="">choose a user</option>
            {users.data?.map((u) => <option key={u.id} value={u.id}>{u.email}</option>)}
          </select>
        )}
        {form.scope === 'api_key' && (
          <input value={form.scope_id} onChange={(e) => setForm({ ...form, scope_id: e.target.value })} placeholder="api key id" className={`${inputSm} font-mono max-w-[16rem]`} />
        )}
        <select value={form.period} onChange={(e) => setForm({ ...form, period: e.target.value })} className={`${inputSm} max-w-[7rem]`}>
          <option value="day">day</option><option value="week">week</option><option value="month">month</option><option value="total">total</option>
        </select>
        <span className="text-sm text-fg-2">$</span>
        <input type="number" step="0.5" min="0" value={form.limit_usd} onChange={(e) => setForm({ ...form, limit_usd: Number(e.target.value) })} className={`${inputSm} max-w-[6rem] tnum`} />
        <select value={form.on_exceed} onChange={(e) => setForm({ ...form, on_exceed: e.target.value })} className={`${inputSm} max-w-[9rem]`}>
          <option value="downgrade">downgrade</option><option value="block">block</option>
        </select>
        <button onClick={() => save.mutate()} className={btn.secondarySm}>Add budget</button>
      </div>
      {err && <Callout kind="error">{err}</Callout>}
    </section>
  )
}

/**
 * The upstreams serving one aggregator model (OpenRouter "endpoints").
 * Adding one creates a separate ws endpoint pinned to that upstream via
 * extra_body.provider, so the same model can be routed to Cerebras, Groq,
 * etc. explicitly instead of letting OpenRouter choose.
 */
function Routes({ pid, model }: { pid: string; model: CatalogModel }) {
  const qc = useQueryClient()
  const routes = useQuery({
    queryKey: ['routes', pid, model.id],
    queryFn: () => api.get<{ routes: CatalogRoute[] }>(`/api/admin/providers/${pid}/routes?model=${encodeURIComponent(model.id)}`),
    staleTime: 60_000,
  })
  const add = useMutation({
    mutationFn: (r: CatalogRoute) =>
      api.put('/api/admin/endpoints', {
        id: r.endpoint_id, provider_id: pid, model_name: model.id,
        display_name: `${model.name} via ${r.provider}`,
        capabilities: r.capabilities, pricing: r.pricing, throughput_class: 'high', latency_class: 'fast',
        extra_body: { provider: { order: [r.tag], allow_fallbacks: false } },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['admin-endpoints'] })
      qc.invalidateQueries({ queryKey: ['models'] })
      qc.invalidateQueries({ queryKey: ['routes', pid, model.id] })
    },
  })
  if (routes.isLoading) return <p className="meta">Loading routes…</p>
  if (routes.error) return <Callout kind="error">{(routes.error as Error).message}</Callout>
  const list = routes.data?.routes ?? []
  if (list.length === 0) return <p className="meta">No per-provider routes listed for this model.</p>
  return (
    <div className="space-y-1">
      <p className="meta">Pin {model.id} to one upstream. Each pinned route is its own endpoint; cheapest output first.</p>
      <ul className="divide-y divide-line-soft text-sm">
        {list.map((r) => (
          <li key={r.tag} className="flex items-center justify-between gap-3 py-1.5">
            <div className="min-w-0">
              <span>{r.provider}</span> <span className="font-mono text-xs text-fg-3">{r.tag}</span>
              <div className="meta">
                {r.capabilities.context_window ? `${Math.round(r.capabilities.context_window / 1000)}k ctx` : 'ctx unknown'}
                {r.capabilities.max_output ? ` · ${Math.round(r.capabilities.max_output / 1000)}k out` : ''}
                {` · $${r.pricing.input_per_m.toFixed(2)} / $${r.pricing.output_per_m.toFixed(2)} per M`}
                {r.quantization && r.quantization !== 'unknown' ? ` · ${r.quantization}` : ''}
                {r.capabilities.tools ? ' · tools' : ' · no tools'}
                {r.uptime_30m ? ` · ${r.uptime_30m.toFixed(1)}% up` : ''}
              </div>
            </div>
            {r.added ? (
              <span className="text-xs text-fg-3 shrink-0">added</span>
            ) : (
              <button onClick={() => add.mutate(r)} disabled={add.isPending} className={`${btn.secondarySm} shrink-0`}>Add route</button>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}

type GitHubStatus = {
  configured: boolean
  fallback_token: boolean
  slug?: string
  install_url?: string
  error?: string
  installations?: { id: number; account: string; type: string; repository_selection: string; html_url: string; suspended: boolean }[]
}

/** GitHub App status: where it's installed, and how to install it. */
function GitHub() {
  const gh = useQuery({ queryKey: ['admin-github'], queryFn: () => api.get<GitHubStatus>('/api/admin/github'), staleTime: 30_000 })
  return (
    <section className="space-y-3">
      <SectionHead aside={gh.data?.configured ? 'app configured' : gh.data?.fallback_token ? 'token fallback' : 'not configured'}>github</SectionHead>
      {gh.data && !gh.data.configured && (
        <p className="reading text-sm text-fg-2">
          No GitHub App is configured. Sandboxes can still clone public repos{gh.data.fallback_token ? ' and push with the GITHUB_TOKEN fallback' : ''}; set <code className="font-mono text-xs">GITHUB_APP_ID</code>, <code className="font-mono text-xs">GITHUB_APP_PRIVATE_KEY</code> and <code className="font-mono text-xs">GITHUB_APP_SLUG</code> in <code className="font-mono text-xs">.env</code> to get repo-scoped push tokens and <code className="font-mono text-xs">open_pr</code>. See <code className="font-mono text-xs">.env.example</code>.
        </p>
      )}
      {gh.data?.configured && (
        <>
          <p className="reading text-sm text-fg-2">
            App <span className="font-mono text-xs">{gh.data.slug}</span>. Code projects whose repo owner has the app installed get push credentials and pull requests automatically.{' '}
            {gh.data.install_url && <a href={gh.data.install_url} target="_blank" rel="noopener" className="underline">Install on another account or org</a>}
          </p>
          {gh.data.error && <Callout kind="error">{gh.data.error}</Callout>}
          <ListGroup empty="Not installed anywhere yet.">
            {gh.data.installations?.map((i) => (
              <li key={i.id} className="flex justify-between gap-3 px-3 py-2">
                <span>{i.account} <span className="text-fg-3">· {i.type.toLowerCase()} · {i.repository_selection} repos</span></span>
                <span className="meta">{i.suspended ? 'suspended' : `#${i.id}`}</span>
              </li>
            ))}
          </ListGroup>
        </>
      )}
    </section>
  )
}
