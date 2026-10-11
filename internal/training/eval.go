package training

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// SmokeEvalExamples is the held-out size under which an eval is a smoke
// result, not a measurement: at N examples one example moves a score by
// 100/N points, so a gap of ten points needs about fifty to mean anything.
// The gate's rule does not change; its message and the Training page say so.
const SmokeEvalExamples = 30

// smokeNote is the sentence appended to a gate message or confirmation when
// the verdict rests on fewer than SmokeEvalExamples examples; "" otherwise.
func smokeNote(examples int) string {
	if examples >= SmokeEvalExamples {
		return ""
	}
	return fmt.Sprintf(" This is a smoke result: the eval had only %d held-out examples, and one example moves the score by %.0f points.", examples, 100/float64(max(examples, 1)))
}

// evalExamples is how many held-out examples an adapter's stored eval scored.
func evalExamples(raw []byte) int {
	var r struct {
		Examples int `json:"examples"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.Examples
}

// EvalResult is what an eval stores on the adapter.
type EvalResult struct {
	Examples      int       `json:"examples"`
	AdapterScore  float64   `json:"adapter_score"`  // mean 0..1
	BaselineScore float64   `json:"baseline_score"` // mean 0..1
	Judge         string    `json:"judge"`
	AdapterWins   int       `json:"adapter_wins"`
	BaselineWins  int       `json:"baseline_wins"`
	Ties          int       `json:"ties"`
	Errors        []string  `json:"errors,omitempty"`
	At            time.Time `json:"at"`
}

// CheckEvaluate reports why an evaluation of the adapter cannot run, with the same errors
// Evaluate returns for it (ErrNotFound, ErrInvalid, ErrNotReady), or nil when it can.
func (s *Service) CheckEvaluate(ctx context.Context, adapterID uuid.UUID) error {
	_, _, err := s.evalInputs(ctx, adapterID)
	return err
}

// QueueEval is what the evaluate button calls: the checks above first, so a click that cannot
// work fails at once with the reason, then the job. An evaluation of the adapter that is already
// queued or running is ErrNotReady with that said, not a second 202.
func (s *Service) QueueEval(ctx context.Context, adapterID uuid.UUID) error {
	if err := s.CheckEvaluate(ctx, adapterID); err != nil {
		return err
	}
	queued, err := s.EnqueueEval(ctx, adapterID)
	if err != nil {
		return err
	}
	if !queued {
		return fmt.Errorf("%w: an evaluation of this adapter is already queued or running", ErrNotReady)
	}
	return nil
}

// evalInputs is every check Evaluate makes before it spends a model call, and the examples it
// then judges. The HTTP handler runs it too, so a click that cannot work fails at once with the
// message the worker would have given, instead of a discarded job nobody sees.
func (s *Service) evalInputs(ctx context.Context, adapterID uuid.UUID) (store.Adapter, []Example, error) {
	ad, err := s.DB.GetAdapter(ctx, adapterID)
	if err != nil {
		return store.Adapter{}, nil, ErrNotFound
	}
	if ad.EndpointID == nil || ad.BaseEndpointID == nil {
		return store.Adapter{}, nil, fmt.Errorf("%w: the adapter needs an endpoint and a base endpoint", ErrInvalid)
	}
	if s.GW == nil {
		return store.Adapter{}, nil, errors.New("training: no gateway")
	}
	if !ad.FinetuneJobID.Valid {
		return store.Adapter{}, nil, fmt.Errorf("%w: the adapter has no job, so no held-out set", ErrInvalid)
	}
	job, err := s.DB.GetFinetuneJob(ctx, ad.FinetuneJobID.UUID)
	if err != nil || !job.DatasetID.Valid {
		return store.Adapter{}, nil, fmt.Errorf("%w: the adapter's dataset is gone", ErrInvalid)
	}
	raw, err := s.DatasetFile(ctx, job.DatasetID.UUID, true)
	if err != nil {
		return store.Adapter{}, nil, fmt.Errorf("%w: the dataset has no held-out examples (holdout_pct was 0 or nothing landed there)", ErrNotReady)
	}
	examples, err := ParseJSONL(raw)
	if err != nil {
		return store.Adapter{}, nil, err
	}
	limit := s.EvalLimit
	if limit <= 0 {
		limit = 40
	}
	if len(examples) > limit {
		examples = examples[:limit]
	}
	if len(examples) == 0 {
		return store.Adapter{}, nil, fmt.Errorf("%w: the held-out set is empty", ErrNotReady)
	}
	return ad, examples, nil
}

// Evaluate is the training.eval job: it runs the dataset's held-out
// examples through the adapter's endpoint and through its base, has a
// judge model score each answer against the reference, and stores both
// means on the adapter. The adapter's endpoint must be reachable: load
// the adapter on the server first (see the adapter's download) and
// enable or promote-by-force its endpoint for the run.
func (s *Service) Evaluate(ctx context.Context, adapterID uuid.UUID) error {
	ad, examples, err := s.evalInputs(ctx, adapterID)
	if err != nil {
		return err
	}
	res := EvalResult{Examples: len(examples), Judge: s.judgeSelector(), At: s.now()}
	var aSum, bSum float64
	for i, ex := range examples {
		prompt, reference := split(ex)
		if reference == "" || len(prompt) == 0 {
			res.Examples--
			continue
		}
		// A recipe adapter is judged under the prompt it was trained with, and
		// its base gets the same prompt: the question is whether fine-tuning
		// beats prompting the base, not whether it beats an unprompted one.
		aText, aErr := s.answer(ctx, *ad.EndpointID, ad.SystemPrompt, prompt)
		bText, bErr := s.answer(ctx, *ad.BaseEndpointID, ad.SystemPrompt, prompt)
		if aErr != nil || bErr != nil {
			res.Examples--
			msg := fmt.Sprintf("example %d:", i)
			if aErr != nil {
				msg += " adapter: " + aErr.Error()
			}
			if bErr != nil {
				msg += " base: " + bErr.Error()
			}
			if len(res.Errors) < 10 {
				res.Errors = append(res.Errors, msg)
			}
			continue
		}
		aScore, err := s.judge(ctx, ad.Rubric, prompt, reference, aText)
		if err != nil {
			return fmt.Errorf("judge: %w", err)
		}
		bScore, err := s.judge(ctx, ad.Rubric, prompt, reference, bText)
		if err != nil {
			return fmt.Errorf("judge: %w", err)
		}
		aSum += aScore
		bSum += bScore
		switch {
		case aScore > bScore:
			res.AdapterWins++
		case bScore > aScore:
			res.BaselineWins++
		default:
			res.Ties++
		}
	}
	if res.Examples <= 0 {
		return fmt.Errorf("%w: no example could be scored (%s)", ErrNotReady, strings.Join(res.Errors, "; "))
	}
	res.AdapterScore = aSum / float64(res.Examples)
	res.BaselineScore = bSum / float64(res.Examples)
	b, _ := json.Marshal(res)
	a32, b32 := float32(res.AdapterScore), float32(res.BaselineScore)
	if err := s.DB.SetAdapterEval(ctx, store.SetAdapterEvalParams{ID: ad.ID, EvalScore: &a32, BaselineScore: &b32, Eval: b}); err != nil {
		return err
	}
	s.log().Info("training: adapter evaluated", "adapter", ad.ID, "adapter_score", res.AdapterScore, "baseline_score", res.BaselineScore, "examples", res.Examples)
	return nil
}

func (s *Service) judgeSelector() string {
	if s.JudgeSelector != "" {
		return s.JudgeSelector
	}
	return "auto"
}

// split separates an example into the prompt turns and the reference
// answer (the last assistant turn's text).
func split(ex Example) ([]gateway.Message, string) {
	n := len(ex.Messages)
	if n < 2 || ex.Messages[n-1].Role != "assistant" {
		return nil, ""
	}
	var msgs []gateway.Message
	for _, m := range ex.Messages[:n-1] {
		role := gateway.Role(m.Role)
		switch m.Role {
		case "user", "assistant":
		case "system":
			continue // the eval prompt carries no system block; both sides see the same turns
		default:
			role = gateway.RoleUser // tool results become user context for a plain comparison
		}
		text := m.Content
		if len(m.ToolCalls) > 0 {
			var calls []string
			for _, c := range m.ToolCalls {
				calls = append(calls, c.Function.Name+"("+c.Function.Arguments+")")
			}
			text = strings.TrimSpace(text + "\n[called " + strings.Join(calls, ", ") + "]")
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == role {
			// Adjacent same-role turns are merged: providers want alternation.
			last := &msgs[len(msgs)-1]
			last.Parts[0].Text += "\n\n" + text
			continue
		}
		msgs = append(msgs, gateway.Message{Role: role, Parts: []gateway.Part{gateway.TextPart(text)}})
	}
	if len(msgs) == 0 || msgs[0].Role != gateway.RoleUser {
		return nil, ""
	}
	return msgs, ex.Messages[n-1].Content
}

func (s *Service) answer(ctx context.Context, endpoint, system string, prompt []gateway.Message) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res, err := s.GW.Complete(cctx, &gateway.Request{Model: endpoint, System: system, Messages: prompt, MaxTokens: 2048, Metadata: gateway.Metadata{TaskClass: gateway.TaskChat}})
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

const judgeSystem = `You grade an answer to a conversation against a reference answer a person approved. Score the candidate from 0 to 10: 10 means as good as or better than the reference in correctness, completeness and tone; 5 means half right or half done; 0 means wrong, empty or harmful. Judge the content, not the length. Reply with JSON only: {"score": <number>, "reason": "<one sentence>"}.`

// judge asks the judge model for a 0..10 score and returns it as 0..1.
func (s *Service) judge(ctx context.Context, rubric string, prompt []gateway.Message, reference, candidate string) (float64, error) {
	var conv strings.Builder
	for _, m := range prompt {
		conv.WriteString(strings.ToUpper(string(m.Role)) + ": " + clip(m.Parts[0].Text, 4000) + "\n\n")
	}
	user := fmt.Sprintf("Conversation:\n%s\nReference answer:\n%s\n\nCandidate answer:\n%s\n\nScore the candidate.", conv.String(), clip(reference, 6000), clip(candidate, 6000))
	// A judge that thinks before it answers (gpt-oss, the default under the
	// classify class) can spend a small budget on its reasoning and return
	// nothing, or a reply cut off in the middle (`{"score":`), so the first
	// try has room and a reply that ran out of tokens gets one more try with
	// a lot.
	var last *gateway.Response
	for _, budget := range []int{judgeTokens, judgeTokens * 3} {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		res, err := s.GW.Complete(cctx, &gateway.Request{
			Model: s.judgeSelector(), System: judgeSystemFor(rubric),
			Messages:  []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart(user)}}},
			MaxTokens: budget, JSON: &gateway.JSONFormat{Name: "score"},
			Metadata: gateway.Metadata{TaskClass: gateway.TaskClassify},
		})
		cancel()
		if err != nil {
			return 0, err
		}
		last = res
		if score, ok := ParseScore(res.Text()); ok {
			return score / 10, nil
		}
		if strings.TrimSpace(res.Text()) != "" && res.FinishReason != gateway.FinishLength {
			break // an answer that is not a score: more room will not fix it
		}
	}
	return 0, fmt.Errorf("the judge returned no score (finish: %s): %q", last.FinishReason, clip(last.Text(), 200))
}

// judgeSystemFor adds a recipe's rubric to the judge's instructions: what
// the person said a good answer is, beyond matching the reference.
func judgeSystemFor(rubric string) string {
	rubric = strings.TrimSpace(rubric)
	if rubric == "" {
		return judgeSystem
	}
	return judgeSystem + "\n\nThe person who set this task wrote how to grade it. Follow it where it speaks, and use the reference answer as an example of a good one:\n" + clip(rubric, 6000)
}

// judgeTokens is the judge's first token budget, reasoning included.
const judgeTokens = 1500

// ParseScore reads {"score": n} out of a judge reply, tolerating prose
// and fences around it; the score is clamped to 0..10.
func ParseScore(text string) (float64, bool) {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			var v struct {
				Score json.Number `json:"score"`
			}
			if err := json.Unmarshal([]byte(text[i:j+1]), &v); err == nil && v.Score != "" {
				if f, err := v.Score.Float64(); err == nil {
					return clamp(f), true
				}
			}
		}
	}
	// A bare number.
	for _, tok := range strings.Fields(strings.NewReplacer(":", " ", ",", " ", "/", " ").Replace(text)) {
		if f, err := strconv.ParseFloat(tok, 64); err == nil {
			return clamp(f), true
		}
	}
	return 0, false
}

func clamp(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 10 {
		return 10
	}
	return f
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
