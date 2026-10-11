/// <reference types="vite/client" />
import { describe, expect, it } from 'vitest'

// The app never opens the browser's own dialogs: confirmations and questions go through
// useConfirm() and usePrompt() (components/dialogs.tsx). This fails on any call to the
// browser's confirm, prompt or alert in web/src, so one cannot creep back in.
const sources = import.meta.glob('./**/*.{ts,tsx}', { query: '?raw', import: 'default', eager: true }) as Record<string, string>

describe('browser dialogs', () => {
  it('are not used anywhere in web/src', () => {
    const found: string[] = []
    for (const [file, text] of Object.entries(sources)) {
      if (file.endsWith('.test.ts') || file.endsWith('.test.tsx')) continue
      text.split('\n').forEach((line, i) => {
        const code = line.replace(/\/\/.*$/, '').replace(/(['"`])(?:\\.|(?!\1).)*\1/g, '""')
        // confirm(...), prompt(...), alert(...) as a global or on window, but not a method such as confirm.mutate(
        if (/(^|[^.\w$])(?:window\s*\.\s*)?(?:confirm|prompt|alert)\(/.test(code) && !/\b(?:const|function|let|var)\s+(?:confirm|prompt|alert)\b/.test(code)) {
          found.push(`${file}:${i + 1}: ${line.trim()}`)
        }
      })
    }
    expect(found).toEqual([])
  })
})
