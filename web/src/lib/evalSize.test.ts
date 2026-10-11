import { describe, expect, it } from 'vitest'
import { DEFAULT_SMOKE_EVAL_EXAMPLES, isSmokeEval, pointsPerExample, smokeTitle } from './evalSize'

describe('smoke evals', () => {
  it('marks 4 examples and not 50, with the line at the threshold', () => {
    expect(isSmokeEval(4)).toBe(true)
    expect(isSmokeEval(50)).toBe(false)
    expect(isSmokeEval(DEFAULT_SMOKE_EVAL_EXAMPLES - 1)).toBe(true)
    expect(isSmokeEval(DEFAULT_SMOKE_EVAL_EXAMPLES)).toBe(false)
    expect(isSmokeEval(40, 50)).toBe(true) // a threshold from the server wins
  })
  it('says how far one example moves the score', () => {
    expect(pointsPerExample(4)).toBe(25)
    expect(pointsPerExample(50)).toBe(2)
    expect(pointsPerExample(0)).toBe(100)
    expect(smokeTitle(4)).toContain('25 points')
  })
})
