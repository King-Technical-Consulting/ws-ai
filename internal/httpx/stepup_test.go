package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/auth"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func newStepUp(t *testing.T, actions string, passkey bool) *stepUp {
	t.Helper()
	modes, err := ParseStepUpActions(actions)
	if err != nil {
		t.Fatal(err)
	}
	return &stepUp{modes: modes, window: 10 * time.Minute, now: func() time.Time { return t0 }, hasPasskey: func(context.Context, uuid.UUID) bool { return passkey }}
}

// step runs one request of a class through the gate and returns the status and decoded body.
func step(t *testing.T, su *stepUp, class string, loc LocalityInfo, p *auth.Principal) (int, map[string]any, bool) {
	t.Helper()
	reached := false
	h := su.require(class)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(200) }))
	req := httptest.NewRequest("POST", "/", nil)
	ctx := req.Context()
	if p != nil {
		ctx = context.WithValue(ctx, principalKey, p)
	}
	req = withLocality(req.WithContext(ctx), loc)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, reached
}

func at(d time.Duration) *time.Time { x := t0.Add(d); return &x }

var (
	home   = LocalityInfo{Configured: true, Locality: Home, Listener: ListenerMain}
	away   = LocalityInfo{Configured: true, Locality: Away, Listener: ListenerTunnel}
	unset  = LocalityInfo{Configured: false, Locality: Home, Listener: ListenerMain}
	member = func(stepped *time.Time) *auth.Principal {
		return &auth.Principal{UserID: uuid.New(), SessionID: uuid.New(), Role: "member", SteppedUpAt: stepped}
	}
)

func TestStepUpGate(t *testing.T) {
	cases := []struct {
		name    string
		actions string
		class   string
		loc     LocalityInfo
		passkey bool
		stepped *time.Time
		want    string // "" passes
	}{
		{"away, never stepped up", "admin", ActAdmin, away, true, nil, codeStepUpRequired},
		{"away, stepped up a minute ago", "admin", ActAdmin, away, true, at(-time.Minute), ""},
		{"away, just inside the window", "admin", ActAdmin, away, true, at(-10*time.Minute + time.Second), ""},
		{"away, exactly at the window's end", "admin", ActAdmin, away, true, at(-10 * time.Minute), codeStepUpRequired},
		{"away, past the window", "admin", ActAdmin, away, true, at(-11 * time.Minute), codeStepUpRequired},
		{"away, no passkey on the account", "admin", ActAdmin, away, false, nil, codeStepUpUnavailable},
		{"away, no passkey, even if a stale mark says otherwise", "admin", ActAdmin, away, false, at(-time.Minute), codeStepUpUnavailable},
		{"home passes", "admin", ActAdmin, home, true, nil, ""},
		{"home, no passkey passes", "admin", ActAdmin, home, false, nil, ""},
		{"unset: inert even away-looking classes", "admin,project_delete:always", ActProjectDelete, unset, true, nil, ""},
		{"a class that is not listed passes", "admin", ActExport, away, true, nil, ""},
		{"none turns it off", "none", ActAdmin, away, true, nil, ""},
		// "always" gates at home too, for anyone who can step up
		{"always, home, never stepped up", "project_delete:always", ActProjectDelete, home, true, nil, codeStepUpRequired},
		{"always, home, stepped up", "project_delete:always", ActProjectDelete, home, true, at(-time.Minute), ""},
		{"always, home, no passkey: unaffected", "project_delete:always", ActProjectDelete, home, false, nil, ""},
		{"always, away, no passkey: refused", "project_delete:always", ActProjectDelete, away, false, nil, codeStepUpUnavailable},
		// a stamp from the future (clock skew between two writes) is not an expiry
		{"a stamp slightly in the future passes", "admin", ActAdmin, away, true, at(2 * time.Second), ""},
	}
	for _, c := range cases {
		code, body, reached := step(t, newStepUp(t, c.actions, c.passkey), c.class, c.loc, member(c.stepped))
		if c.want == "" {
			if code != 200 || !reached {
				t.Errorf("%s: want pass, got %d %v", c.name, code, body)
			}
			continue
		}
		if code != 403 || reached || body["code"] != c.want {
			t.Errorf("%s: want 403 %s, got %d %v reached=%v", c.name, c.want, code, body, reached)
		}
	}
}

func TestStepUpResponseShape(t *testing.T) {
	code, body, _ := step(t, newStepUp(t, "provider_key", true), ActProviderKey, away, member(nil))
	if code != 403 {
		t.Fatalf("status %d", code)
	}
	// exactly the documented shape
	if len(body) != 4 || body["error"] != "step-up required: confirm with your passkey" || body["code"] != "step_up_required" || body["action"] != "provider_key" || body["window_seconds"] != float64(600) {
		t.Errorf("body = %v", body)
	}
	_, body, _ = step(t, newStepUp(t, "provider_key", false), ActProviderKey, away, member(nil))
	if body["code"] != "step_up_unavailable" || !strings.Contains(body["error"].(string), "add a passkey to do this away from home") {
		t.Errorf("unavailable body = %v", body)
	}
	// a passkey cannot be added from away without one: the message says so
	_, body, _ = step(t, newStepUp(t, "passkey", false), ActPasskey, away, member(nil))
	if !strings.Contains(body["error"].(string), "only be added or removed from home") {
		t.Errorf("passkey body = %v", body)
	}
}

func TestStepUpIgnoresWhatItDoesNotGovern(t *testing.T) {
	su := newStepUp(t, "admin", true)
	// no principal: the auth middleware answers, not this one
	if code, _, reached := step(t, su, ActAdmin, away, nil); code != 200 || !reached {
		t.Errorf("no principal: %d", code)
	}
	// an API key never reaches these routes; the gate does not pretend otherwise
	key := &auth.Principal{UserID: uuid.New(), APIKeyID: uuid.NullUUID{UUID: uuid.New(), Valid: true}}
	if code, _, _ := step(t, su, ActAdmin, away, key); code != 200 {
		t.Errorf("api key: %d", code)
	}
}

func TestStepUpAdminWritesAreGatedReadsAreNot(t *testing.T) {
	su := newStepUp(t, "admin,budget", true)
	h := su.requireAdminWrites(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	run := func(method, path string) (int, string) {
		req := httptest.NewRequest(method, path, nil)
		req = withLocality(req.WithContext(context.WithValue(req.Context(), principalKey, member(nil))), away)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var b map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		a, _ := b["action"].(string)
		return rec.Code, a
	}
	for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
		if code, _ := run(m, "/api/admin/usage"); code != 200 {
			t.Errorf("%s is a read: %d", m, code)
		}
	}
	for _, m := range []string{"PUT", "POST", "DELETE", "PATCH"} {
		if code, a := run(m, "/api/admin/providers"); code != 403 || a != "admin" {
			t.Errorf("%s admin write: %d %q", m, code, a)
		}
		if code, a := run(m, "/api/admin/budgets/abc"); code != 403 || a != "budget" {
			t.Errorf("%s budget write: %d %q", m, code, a)
		}
	}
}

func TestParseStepUpActions(t *testing.T) {
	m, err := ParseStepUpActions("")
	if err != nil || len(m) != len(stepUpClasses) || m[ActProjectDelete] != modeAlways || m[ActAdmin] != modeAway || m[ActTraining] != modeAway || m[ActConversationDelete] != modeAway {
		t.Fatalf("default: %v %v", m, err)
	}
	// every class is in the default, and only project_delete is "always"
	for _, c := range stepUpClasses {
		if m[c] == 0 {
			t.Errorf("%s is not in the default", c)
		}
		if (m[c] == modeAlways) != (c == ActProjectDelete) {
			t.Errorf("%s: mode %v", c, m[c])
		}
	}
	if m, err := ParseStepUpActions("NONE"); err != nil || len(m) != 0 {
		t.Errorf("none: %v %v", m, err)
	}
	m, err = ParseStepUpActions(" admin , export:always,admin:away, api_key:away")
	if err != nil || m[ActAdmin] != modeAway || m[ActExport] != modeAlways || m[ActAPIKey] != modeAway || len(m) != 3 {
		t.Errorf("explicit: %v %v", m, err)
	}
	// "always" is not weakened by a later "away" for the same class
	if m, _ := ParseStepUpActions("export:always,export:away"); m[ActExport] != modeAlways {
		t.Errorf("always then away: %v", m)
	}
	for _, bad := range []string{"adminn", "admin:sometimes", "admin,nope", ",", "provider-key"} {
		if _, err := ParseStepUpActions(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		} else if strings.Contains(bad, "nope") || bad == "adminn" {
			if !strings.Contains(err.Error(), "classes:") {
				t.Errorf("%q: the error should list the classes: %v", bad, err)
			}
		}
	}
}

func TestParseStepUpWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 10 * time.Minute, "10m": 10 * time.Minute, "90s": 90 * time.Second, "1h": time.Hour, "24h": 24 * time.Hour} {
		if got, err := ParseStepUpWindow(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"ten", "10", "30s", "25h", "-5m", "0"} {
		if _, err := ParseStepUpWindow(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestStepUpUntil(t *testing.T) {
	su := newStepUp(t, "admin", true)
	if !su.until(nil).IsZero() || !su.until(at(-11*time.Minute)).IsZero() {
		t.Error("never, or expired: no until")
	}
	if u := su.until(at(-3 * time.Minute)); !u.Equal(t0.Add(7 * time.Minute)) {
		t.Errorf("until = %v", u)
	}
}

func TestStepUpBeginsAreLimited(t *testing.T) {
	var b stepUpBegins
	u, other := uuid.New(), uuid.New()
	for i := 0; i < 10; i++ {
		if !b.allow(u, t0) {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if b.allow(u, t0.Add(30*time.Second)) {
		t.Error("an eleventh attempt in a minute")
	}
	if !b.allow(other, t0) {
		t.Error("another person is not limited by this one")
	}
	if !b.allow(u, t0.Add(61*time.Second)) {
		t.Error("the window did not reopen")
	}
}
