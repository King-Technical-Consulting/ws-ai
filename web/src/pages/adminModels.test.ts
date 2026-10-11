import { describe, expect, it } from 'vitest'
import { endpointMatches, filtersActive, groupEndpoints, healthSummary, type FindableEndpoint, type ModelFilters } from './adminModels'

const NONE: ModelFilters = { q: '', provider: '', enabled: '', routable: '', where: '', health: '', tier: '', pq: '' }

const ep = (id: string, over: Partial<FindableEndpoint> = {}): FindableEndpoint => {
  const [provider, model] = id.split('/')
  return { id, display_name: model, provider_id: provider, model_name: model, enabled: true, local: false, health: { status: 'healthy' }, ...over }
}

const ALL = [
  ep('local-llama/qwen3-14b', { local: true, tier: 'medium' }),
  ep('local-llama/gpt-oss-20b', { local: true, tier: 'medium', enabled: false }),
  ep('local-llama/qwen3-0.6b', { local: true, tier: 'small', not_routable: true, health: { status: 'down' } }),
  ep('openrouter/deepseek-chat', { tier: 'large', health: { status: 'degraded' } }),
  ep('anthropic/claude-sonnet', { tier: 'large' }),
]

describe('endpointMatches', () => {
  it('matches everything when nothing is set', () => {
    expect(ALL.every((e) => endpointMatches(e, NONE))).toBe(true)
  })
  it('searches id, name, provider, model and tier, every word having to match', () => {
    const hits = (q: string) => ALL.filter((e) => endpointMatches(e, { ...NONE, q })).map((e) => e.id)
    expect(hits('qwen')).toEqual(['local-llama/qwen3-14b', 'local-llama/qwen3-0.6b'])
    expect(hits('QWEN small')).toEqual(['local-llama/qwen3-0.6b'])
    expect(hits('openrouter')).toEqual(['openrouter/deepseek-chat'])
    expect(hits('large')).toEqual(['openrouter/deepseek-chat', 'anthropic/claude-sonnet'])
    expect(hits('nothing like this')).toEqual([])
  })
  it('combines the filters with each other and with the search', () => {
    const hits = (f: Partial<ModelFilters>) => ALL.filter((e) => endpointMatches(e, { ...NONE, ...f })).map((e) => e.id)
    expect(hits({ enabled: 'off' })).toEqual(['local-llama/gpt-oss-20b'])
    expect(hits({ routable: 'no' })).toEqual(['local-llama/qwen3-0.6b'])
    expect(hits({ where: 'hosted' })).toEqual(['openrouter/deepseek-chat', 'anthropic/claude-sonnet'])
    expect(hits({ health: 'down' })).toEqual(['local-llama/qwen3-0.6b'])
    expect(hits({ tier: 'large', where: 'hosted', health: 'degraded' })).toEqual(['openrouter/deepseek-chat'])
    expect(hits({ where: 'local', q: 'qwen', enabled: 'on' })).toEqual(['local-llama/qwen3-14b', 'local-llama/qwen3-0.6b'])
    expect(hits({ provider: 'local-llama', q: 'oss' })).toEqual(['local-llama/gpt-oss-20b'])
    expect(hits({ tier: 'small', where: 'hosted' })).toEqual([])
  })
  it('does not treat an endpoint with no tier as any tier', () => {
    expect(endpointMatches(ep('x/y'), { ...NONE, tier: 'medium' })).toBe(false)
  })
})

describe('groupEndpoints', () => {
  it('groups by provider, providers by id, enabled first and then by name', () => {
    const g = groupEndpoints(ALL, NONE)
    expect(g.map((x) => x.provider)).toEqual(['anthropic', 'local-llama', 'openrouter'])
    expect(g[1].rows.map((e) => e.id)).toEqual(['local-llama/qwen3-0.6b', 'local-llama/qwen3-14b', 'local-llama/gpt-oss-20b'])
    expect(g[1].total).toBe(3)
  })
  it('counts health over the whole group, not only the matches', () => {
    const g = groupEndpoints(ALL, { ...NONE, q: 'gpt-oss' }).find((x) => x.provider === 'local-llama')!
    expect(g.rows).toHaveLength(1)
    expect(g.total).toBe(3)
    expect(g.health).toEqual({ healthy: 2, degraded: 0, down: 1 })
  })
  it('hides a group with no match while filtering, and keeps every group when not', () => {
    expect(groupEndpoints(ALL, { ...NONE, q: 'deepseek' }).map((x) => x.provider)).toEqual(['openrouter'])
    expect(groupEndpoints(ALL, NONE)).toHaveLength(3)
  })
  it('keeps a provider with no endpoints out of the groups (it has no rows to list)', () => {
    expect(groupEndpoints([], NONE)).toEqual([])
  })
})

describe('the small helpers', () => {
  it('says what is being filtered', () => {
    expect(filtersActive(NONE)).toBe(false)
    expect(filtersActive({ ...NONE, pq: 'x' })).toBe(false) // the provider search is not an endpoint filter
    expect(filtersActive({ ...NONE, tier: 'small' })).toBe(true)
    expect(filtersActive({ ...NONE, provider: 'openrouter' })).toBe(true)
  })
  it('summarises health, leaving out zeros', () => {
    expect(healthSummary({ healthy: 2, degraded: 0, down: 1 })).toBe('2 healthy · 1 down')
    expect(healthSummary({ healthy: 0, degraded: 0, down: 0 })).toBe('')
  })
})
