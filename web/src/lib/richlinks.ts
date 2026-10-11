// Rich links in replies (PLAN M13 cut 6): [word](tip: a short explanation)
// shows a tooltip, [label](src: <source id> p.3) jumps to a notebook source.
// Neither is a URL scheme the markdown sanitizer would pass, and spaces are
// not allowed in a link destination, so both are rewritten to inert https
// placeholders before rendering and recognised again by the link component.
const BASE = 'https://rich.ws.invalid'

export function rewriteRichLinks(md: string): string {
  return md
    .replace(/\]\(\s*tip:\s*([^)\n]*)\)/g, (_m, t: string) => `](${BASE}/tip#${encodeURIComponent(t.trim())})`)
    .replace(
      /\]\(\s*src:\s*([0-9a-fA-F-]{36})(?:\s*(?:p\.?|page)\s*(\d+))?\s*\)/g,
      (_m, id: string, p?: string) => `](${BASE}/src/${id.toLowerCase()}${p ? `#p${p}` : ''})`,
    )
}

export type RichHref = { kind: 'tip'; text: string } | { kind: 'src'; id: string; page?: number }

export function parseRichHref(href: string | undefined): RichHref | null {
  if (!href || !href.startsWith(BASE + '/')) return null
  const rest = href.slice(BASE.length + 1)
  if (rest.startsWith('tip#')) {
    try {
      return { kind: 'tip', text: decodeURIComponent(rest.slice(4)) }
    } catch {
      return { kind: 'tip', text: rest.slice(4) }
    }
  }
  const m = /^src\/([0-9a-f-]{36})(?:#p(\d+))?$/.exec(rest)
  return m ? { kind: 'src', id: m[1], page: m[2] ? parseInt(m[2], 10) : undefined } : null
}
