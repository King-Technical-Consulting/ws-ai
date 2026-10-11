package httpx

import (
	"testing"
	"time"

	"github.com/jking323/ws/internal/store"
)

func ev(kind, detail string, at time.Time) store.SandboxEvent {
	return store.SandboxEvent{Kind: kind, Detail: []byte(detail), CreatedAt: at}
}

func TestLastSandboxError(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	pull := ev("error", `{"stage":"pull","image":"x","error":"could not pull x: unauthorized"}`, t0)
	clone := ev("error", `{"stage":"clone","error":"private repository and no GitHub credential is configured"}`, t0.Add(time.Minute))

	cases := []struct {
		name   string
		events []store.SandboxEvent // newest first, as the query returns them
		want   string               // the error text, "" for none
		stage  string
	}{
		{"no events", nil, "", ""},
		{"a pull failure is current", []store.SandboxEvent{pull, ev("created", `{}`, t0.Add(-time.Minute))}, "could not pull x: unauthorized", "pull"},
		{"a start after the failure clears it", []store.SandboxEvent{ev("started", `{}`, t0.Add(time.Hour)), pull}, "", ""},
		{"a clone failure after the start is current", []store.SandboxEvent{clone, ev("started", `{}`, t0), pull}, "private repository and no GitHub credential is configured", "clone"},
		{"a successful clone after the failure clears it", []store.SandboxEvent{ev("clone", `{"repo":"r"}`, t0.Add(time.Hour)), clone}, "", ""},
		{"exec and stopped say nothing about setup", []store.SandboxEvent{ev("stopped", `{}`, t0.Add(time.Hour)), ev("exec", `{}`, t0.Add(time.Hour)), pull}, "could not pull x: unauthorized", "pull"},
		{"bad detail still reports the error with no text", []store.SandboxEvent{ev("error", `not json`, t0)}, "", ""},
	}
	for _, c := range cases {
		got := lastSandboxError(c.events)
		switch {
		case c.name == "bad detail still reports the error with no text":
			if got == nil || got.Error != "" {
				t.Errorf("%s: got %+v", c.name, got)
			}
		case c.want == "":
			if got != nil {
				t.Errorf("%s: got %+v, want none", c.name, got)
			}
		default:
			if got == nil || got.Error != c.want || got.Stage != c.stage {
				t.Errorf("%s: got %+v, want %q at %s", c.name, got, c.want, c.stage)
			}
		}
	}
}
