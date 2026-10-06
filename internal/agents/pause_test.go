package agents

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

// TestPauseResume: a queued or running run can be held and released; a
// finished or already-held one cannot be paused, and only a held one
// resumes. A held run counts as open for steering.
func TestPauseResume(t *testing.T) {
	svc, db, _, ag := newService(t)
	ctx := context.Background()
	run, err := svc.Start(ctx, StartParams{AgentID: ag.ID, Input: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Resume(ctx, run.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("resume of a queued run: %v", err)
	}
	if err := svc.Pause(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetRun(ctx, run.ID); got.Status != "paused_manual" {
		t.Errorf("status after pause = %s", got.Status)
	}
	if err := svc.Pause(ctx, run.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("pausing twice: %v", err)
	}
	if _, err := svc.Steer(ctx, run.ID, "hello", ag.OwnerID); !errors.Is(err, ErrBusy) {
		t.Errorf("steer while paused: %v", err)
	}
	if err := svc.Resume(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetRun(ctx, run.ID); got.Status != "queued" {
		t.Errorf("status after resume = %s", got.Status)
	}
	_ = svc.Cancel(ctx, run.ID)
	if err := svc.Pause(ctx, run.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("pausing a cancelled run: %v", err)
	}
}

// TestHookSignature: a delivery that carries X-Hub-Signature-256 must be
// signed with the trigger's secret; one without the header is taken on
// the URL secret alone.
func TestHookSignature(t *testing.T) {
	svc, _, _, ag := newService(t)
	ctx := context.Background()
	trig, secret, err := svc.NewTrigger(ctx, ag.ID, KindRepoPush, "ci", json.RawMessage(`{"repo":"acme/ws"}`))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(key, body string) string {
		m := hmac.New(sha256.New, []byte(key))
		m.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	if !VerifySignature(sign(secret, pushPayload), secret, pushPayload) {
		t.Error("a good signature should verify")
	}
	for _, bad := range []string{"", "sha256=", "sha256=zz", sign("other", pushPayload), sign(secret, pushPayload+" "), "sha1=abcd"} {
		if VerifySignature(bad, secret, pushPayload) {
			t.Errorf("signature %q should not verify", bad)
		}
	}
	// Wrong signature: refused like a wrong secret, no run.
	if _, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, GitHubEvent: "push", Body: pushPayload, Signature: sign("other", pushPayload)}); !errors.Is(err, ErrSecret) {
		t.Errorf("bad signature: %v", err)
	}
	// Right signature: a run.
	if _, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, GitHubEvent: "push", Body: pushPayload, Signature: sign(secret, pushPayload)}); err != nil {
		t.Errorf("good signature: %v", err)
	}
}
