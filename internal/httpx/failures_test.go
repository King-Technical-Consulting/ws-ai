package httpx

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFailureReference(t *testing.T) {
	run := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	agent := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	args := func(s string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(s), &m)
		return m
	}
	cases := []struct {
		kind, args, label, link string
	}{
		{"agent.run", `{"run_id":"` + run.String() + `"}`, "agent run 11111111", "/agents/" + agent.String()},
		{"agent.resume", `{"run_id":"` + run.String() + `"}`, "agent run 11111111", "/agents/" + agent.String()},
		{"agent.run", `{"run_id":"99999999-2222-3333-4444-555555555555"}`, "agent run 99999999", "/agents"},
		{"media.generate", `{"job_id":"abcdef0123456789"}`, "media job abcdef01", "/media"},
		{"training.build", `{"dataset_id":"abcdef0123456789"}`, "dataset abcdef01", "/training"},
		{"training.finetune", `{"job_id":"abcdef0123456789"}`, "fine-tune abcdef01", "/training"},
		{"training.eval", `{"adapter_id":"abcdef0123456789"}`, "adapter abcdef01", "/training"},
		{"conversation.compact", `{"conversation_id":"abcdef0123456789"}`, "conversation abcdef01", "/c/abcdef0123456789"},
	}
	for _, c := range cases {
		got := failureReference(c.kind, args(c.args), map[uuid.UUID]uuid.UUID{run: agent})
		if got == nil || got.Label != c.label || got.Link != c.link {
			t.Errorf("%s %s: got %+v, want %q %q", c.kind, c.args, got, c.label, c.link)
		}
	}
	for _, kind := range []string{"agent.reap", "rental.reconcile", "auth.purge", "unknown.kind"} {
		if got := failureReference(kind, args(`{}`), nil); got != nil {
			t.Errorf("%s: want no reference, got %+v", kind, got)
		}
	}
}

func TestLastJobError(t *testing.T) {
	if got := lastJobError([]byte(`{"at":"2026-10-10T00:00:00Z","attempt":3,"error":"endpoint down","trace":"..."}`)); got != "endpoint down" {
		t.Errorf("got %q", got)
	}
	if got := lastJobError(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	long := lastJobError([]byte(`{"error":"` + strings.Repeat("é", 700) + `"}`))
	if r := []rune(long); len(r) != maxFailureText+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("long error not cut at %d runes: %d", maxFailureText, len(r))
	}
}
