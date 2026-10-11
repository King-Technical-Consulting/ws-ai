import { describe, expect, it } from 'vitest'
import { stepUpNote } from './LocalityBadge'
import type { Locality } from '../api'

const info = (locality: 'home' | 'away', configured = true): Locality => ({ configured, locality, listener: 'main', address: '', reason: '' })
const now = Date.parse('2026-10-10T12:00:00Z')

describe('what step-up means from here', () => {
  it('says nothing at home or when home and away are not judged', () => {
    expect(stepUpNote(info('home'), null, true, now)).toBe('')
    expect(stepUpNote(info('away', false), null, true, now)).toBe('')
  })
  it('asks for a tap when away and not unlocked', () => {
    expect(stepUpNote(info('away'), null, true, now)).toBe('a passkey tap is needed for sensitive actions')
    expect(stepUpNote(info('away'), '2026-10-10T11:59:00Z', true, now)).toBe('a passkey tap is needed for sensitive actions')
  })
  it('counts the minutes left, rounding up', () => {
    expect(stepUpNote(info('away'), '2026-10-10T12:07:00Z', true, now)).toBe('unlocked for 7 more minutes')
    expect(stepUpNote(info('away'), '2026-10-10T12:00:20Z', true, now)).toBe('unlocked for 1 more minute')
  })
  it('refuses, in words, when the account has no passkey', () => {
    expect(stepUpNote(info('away'), null, false, now)).toContain('refused from here')
    expect(stepUpNote(info('away'), '2026-10-10T12:07:00Z', false, now)).toContain('refused from here')
  })
})
