// An eval on fewer held-out examples than this is a smoke result, not a measurement. The server
// owns the number (training.SmokeEvalExamples) and sends it with the adapters list; this is the
// value used until that arrives.
export const DEFAULT_SMOKE_EVAL_EXAMPLES = 30

export const isSmokeEval = (examples: number, threshold = DEFAULT_SMOKE_EVAL_EXAMPLES) => examples < threshold

/** How far one example moves the score, in percentage points. */
export const pointsPerExample = (examples: number) => Math.round(100 / Math.max(examples, 1))

export const smokeTitle = (examples: number, threshold = DEFAULT_SMOKE_EVAL_EXAMPLES) =>
  `Fewer than ${threshold} held-out examples: one example moves the score by ${pointsPerExample(examples)} points, so this shows the pipeline works, not how good the adapter is.`
