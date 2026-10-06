import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ModelsResponse } from '../api'
import { registerPasskey } from '../lib/passkey'
import { SectionHead, PageTitle, Callout, ListGroup, Row, btn, inputSm } from '../components/ui'
import { useMe } from '../App'

type Passkey = { id: string; name: string; created_at: string; last_used_at: string | null }
type Key = { id: string; name: string; prefix: string; scopes: string[]; default_policy: string; created_at: string; last_used_at: string | null }

// Policies a key can default to: "auto" (the rules) plus the aliases the
// routing policy defines (best, cheap, local, code, …). A client that
// sends no model, or an unknown one, is routed by the key's policy.
const BUILTIN_POLICIES = ['auto']

// envBlock is what a Claude Code user exports to use ws as its gateway.
// The optional lines matter when the key's policy routes to local models:
// Claude Code's experimental betas only work on hosted Anthropic, and its
// default model names must be pinned to ws selectors.
function envBlock(key: string, policy: string, origin: string) {
  const lines = [
    '# Claude Code through ws (API billing, ws routing and ledger)',
    `export ANTHROPIC_BASE_URL=${origin}`,
    `export ANTHROPIC_AUTH_TOKEN=${key}`,
    '# optional: list ws models in /model',
    'export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1',
  ]
  if (policy !== 'auto' && policy !== 'best') {
    lines.push(
      `# the "${policy}" policy can land on local models: disable hosted-only betas and pin the model names`,
      'export CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1',
      `export ANTHROPIC_DEFAULT_OPUS_MODEL=${policy}`,
      `export ANTHROPIC_DEFAULT_SONNET_MODEL=${policy}`,
      `export ANTHROPIC_DEFAULT_HAIKU_MODEL=${policy}`,
    )
  }
  return lines.join('\n')
}

// mcpAddLine registers ws as an MCP server in Claude Code (Streamable
// HTTP at /mcp, the same key as a bearer).
function mcpAddLine(key: string, origin: string) {
  return `claude mcp add --transport http ws ${origin}/mcp --header "Authorization: Bearer ${key}"`
}

export default function Settings() {
  const qc = useQueryClient()
  const passkeys = useQuery({ queryKey: ['passkeys'], queryFn: () => api.get<Passkey[]>('/api/auth/passkeys') })
  const keys = useQuery({ queryKey: ['keys'], queryFn: () => api.get<Key[]>('/api/keys') })
  const models = useQuery({ queryKey: ['models'], queryFn: () => api.get<ModelsResponse>('/api/models') })
  const [newKey, setNewKey] = useState<{ key: string; policy: string; mcp: boolean; jobs: boolean } | null>(null)
  const [keyName, setKeyName] = useState('')
  const [policy, setPolicy] = useState('auto')
  const [mcp, setMCP] = useState(false)
  const [jobs, setJobs] = useState(false)
  const [copied, setCopied] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const policies = [...BUILTIN_POLICIES, ...Object.keys(models.data?.aliases ?? {}).filter((a) => !BUILTIN_POLICIES.includes(a)).sort()]
  const origin = window.location.origin

  const addPasskey = useMutation({
    mutationFn: () => registerPasskey(navigator.platform || 'Device'),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['passkeys'] }),
    onError: (e) => setErr(e.message),
  })
  const delPasskey = useMutation({
    mutationFn: (id: string) => api.del(`/api/auth/passkeys/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['passkeys'] }),
  })
  const createKey = useMutation({
    mutationFn: () =>
      api.post<{ key: string }>('/api/keys', {
        name: keyName || 'default',
        default_policy: policy,
        scopes: ['chat', ...(mcp ? ['mcp'] : []), ...(jobs ? ['jobs'] : [])],
      }),
    onSuccess: (r) => {
      setNewKey({ key: r.key, policy, mcp, jobs })
      setCopied(false)
      setKeyName('')
      qc.invalidateQueries({ queryKey: ['keys'] })
    },
    onError: (e) => setErr(e.message),
  })
  const revokeKey = useMutation({
    mutationFn: (id: string) => api.del(`/api/keys/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['keys'] }),
  })
  const me = useMe()
  const consent = useMutation({
    mutationFn: (enabled: boolean) => api.put('/api/me/training-consent', { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['me'] }),
    onError: (e) => setErr(e.message),
  })

  const copyEnv = async () => {
    if (!newKey) return
    try {
      await navigator.clipboard.writeText(envBlock(newKey.key, newKey.policy, origin))
      setCopied(true)
    } catch {
      setCopied(false)
    }
  }

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-2xl p-6 space-y-10">
        <PageTitle>Settings</PageTitle>

        <section className="space-y-3">
          <SectionHead>passkeys</SectionHead>
          <p className="text-sm text-fg-2">Sign in without email. Add one per device.</p>
          <ListGroup empty="No passkeys yet.">
            {passkeys.data?.map((p) => (
              <Row key={p.id} action={<button onClick={() => delPasskey.mutate(p.id)} className={btn.danger}>Remove</button>}>
                {p.name} <span className="meta">· added {new Date(p.created_at).toLocaleDateString()}</span>
              </Row>
            ))}
          </ListGroup>
          <button onClick={() => addPasskey.mutate()} className={btn.primarySm}>Add passkey for this device</button>
        </section>

        <section className="space-y-3">
          <SectionHead>training</SectionHead>
          <p className="text-sm text-fg-2">
            The owner can build fine-tuning datasets from conversations and the thumbs people give answers. Yours are only ever included when this is on; turning it off keeps new datasets from reading them.
          </p>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" className="accent-accent" checked={!!me.data?.training_consent} disabled={consent.isPending} onChange={(e) => consent.mutate(e.target.checked)} /> Allow my conversations in training datasets
          </label>
        </section>

        <section className="space-y-3">
          <SectionHead>api keys</SectionHead>
          <p className="text-sm text-fg-2">
            Use the platform from Claude Code, Cursor, or any OpenAI or Anthropic compatible client. A key's policy decides where a request goes
            when the client names no model, or one ws doesn't know; hosted Claude requests pass through to Anthropic byte for byte.
          </p>
          {newKey && (
            <Callout>
              <p className="mb-1 text-fg-2">Copy this key now. It won't be shown again.</p>
              <code className="font-mono text-xs break-all select-all">{newKey.key}</code>
              <p className="mt-3 mb-1 text-fg-2">For Claude Code, export these in the shell that runs it:</p>
              <pre className="font-mono text-xs whitespace-pre-wrap break-all select-all rounded-lg border border-line bg-bg-2 p-3">
                {envBlock(newKey.key, newKey.policy, origin)}
              </pre>
              <div className="mt-2 flex items-center gap-3">
                <button onClick={copyEnv} className={btn.secondarySm}>Copy env block</button>
                {copied && <span className="text-sm text-fg-2">Copied.</span>}
              </div>
              {newKey.mcp && (
                <>
                  <p className="mt-3 mb-1 text-fg-2">To give Claude Code ws's own tools (models, runs, conversations, the task router) as an MCP server:</p>
                  <pre className="font-mono text-xs whitespace-pre-wrap break-all select-all rounded-lg border border-line bg-bg-2 p-3">
                    {mcpAddLine(newKey.key, origin)}
                  </pre>
                </>
              )}
              {newKey.jobs && (
                <p className="mt-3 text-fg-2">
                  For <code className="font-mono text-xs">wsj</code>, put the key on one line in the file its <code className="font-mono text-xs">[ws] key_file</code> names, or export it as <code className="font-mono text-xs">WSJ_WS_KEY</code>.
                </p>
              )}
            </Callout>
          )}
          <ListGroup empty="No API keys.">
            {keys.data?.map((k) => (
              <Row key={k.id} action={<button onClick={() => revokeKey.mutate(k.id)} className={btn.danger}>Revoke</button>}>
                {k.name} <span className="font-mono text-xs text-fg-3">{k.prefix}…</span>
                <span className="meta"> · policy {k.default_policy || 'auto'} · {k.scopes.join(', ') || 'no scope'}</span>
                {k.last_used_at && <span className="meta"> · used {new Date(k.last_used_at).toLocaleDateString()}</span>}
              </Row>
            ))}
          </ListGroup>
          <div className="flex gap-2">
            <input value={keyName} onChange={(e) => setKeyName(e.target.value)} placeholder="Key name" className={inputSm} />
            <select value={policy} onChange={(e) => setPolicy(e.target.value)} className={inputSm} aria-label="Default policy">
              {policies.map((p) => (
                <option key={p} value={p}>{p}</option>
              ))}
            </select>
            <button onClick={() => createKey.mutate()} className={btn.secondarySm}>Create key</button>
          </div>
          <label className="flex items-center gap-2 text-xs text-fg-2">
            <input type="checkbox" className="accent-accent" checked={mcp} onChange={(e) => setMCP(e.target.checked)} /> MCP access: the key may also drive ws as an MCP server (/mcp).
          </label>
          <label className="flex items-center gap-2 text-xs text-fg-2">
            <input type="checkbox" className="accent-accent" checked={jobs} onChange={(e) => setJobs(e.target.checked)} /> Job reporting: <code className="font-mono">wsj</code> may report Claude Code sessions with it (owner only).
          </label>
          <p className="text-xs text-fg-3">A key reaches only what its boxes say: /v1 always, and never the rest of this app, which takes your sign-in.</p>
          {err && <Callout kind="error">{err}</Callout>}
        </section>
      </div>
    </div>
  )
}
