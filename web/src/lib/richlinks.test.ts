import { describe, expect, it } from 'vitest'
import { parseRichHref, rewriteRichLinks } from './richlinks'

const ID = '0f4d3b1a-0000-4000-8000-123456789abc'

describe('rich links', () => {
  it('rewrites a tip with spaces into a link the sanitizer passes', () => {
    const out = rewriteRichLinks('A [COP](tip: heat out per unit of electricity in) figure.')
    expect(out).not.toContain('tip:')
    const href = /\]\(([^)]+)\)/.exec(out)![1]
    expect(parseRichHref(href)).toEqual({ kind: 'tip', text: 'heat out per unit of electricity in' })
  })

  it('rewrites a source link with and without a page', () => {
    const out = rewriteRichLinks(`See [one](src: ${ID} p.3) and [two](src:${ID}).`)
    const hrefs = [...out.matchAll(/\]\(([^)]+)\)/g)].map((m) => parseRichHref(m[1]))
    expect(hrefs).toEqual([
      { kind: 'src', id: ID, page: 3 },
      { kind: 'src', id: ID, page: undefined },
    ])
  })

  it('leaves ordinary links and other text alone', () => {
    const md = 'A [link](https://example.com/a) and (tip: not a link) and src: plain.'
    expect(rewriteRichLinks(md)).toBe(md)
    expect(parseRichHref('https://example.com/a')).toBeNull()
    expect(parseRichHref(undefined)).toBeNull()
  })

  it('does not trust a placeholder that is not shaped like ours', () => {
    expect(parseRichHref('https://rich.ws.invalid/src/not-a-uuid')).toBeNull()
  })
})
