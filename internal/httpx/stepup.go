package httpx

// Step-up (PLAN M5, docs/design/m5-stepup-home-away.md). A sensitive action
// made from away needs a passkey assertion made in the last few minutes
// (WS_STEPUP_WINDOW). One assertion is remembered on the browser session and
// unlocks every gated class until the window runs out. Nothing here applies
// until WS_HOME_NETWORKS is set: with it unset every request is home and the
// whole feature is inert, whatever WS_STEPUP_ACTIONS says.
//
// Action classes. The first six are the design's; passkey is added because
// without it a stolen session cookie used from away could register its own
// passkey and then step up with it.
//
//	admin           writes under /api/admin (except budgets)
//	budget          writes under /api/admin/budgets
//	provider_key    saving or removing a person's own provider key
//	api_key         minting an API key
//	export          exporting data (the artifact export)
//	passkey         adding or removing a passkey
//	training        promoting, force-promoting or unpromoting an adapter,
//	                deleting one, and changing the training routes: the
//	                destructive and live-changing actions of the Training page
//	conversation_delete  deleting a conversation permanently (the route comes
//	                with the archive-then-delete work; the class waits for it)
//	project_delete  deleting a project permanently, "always" (also at home); the
//	                route comes with the same work
//
// A class is gated when away, or also at home when its policy is "always".
// A user with no passkey cannot step up: away they are refused
// (step_up_unavailable); at home they are unaffected, "always" included.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Step-up action classes.
const (
	ActAdmin       = "admin"
	ActBudget      = "budget"
	ActProviderKey = "provider_key"
	ActAPIKey      = "api_key"
	ActExport      = "export"
	ActPasskey     = "passkey"
	ActTraining    = "training"
	// ActConversationDelete gates the permanent delete of a chat and "delete all archived";
	// ActProjectDelete (always, even at home) gates the permanent delete of a project
	// (handlers_archive.go, wired in server.go).
	ActConversationDelete = "conversation_delete"
	ActProjectDelete      = "project_delete"
)

var stepUpClasses = []string{ActAdmin, ActBudget, ActProviderKey, ActAPIKey, ActExport, ActPasskey, ActTraining, ActConversationDelete, ActProjectDelete}

// DefaultStepUpActions is WS_STEPUP_ACTIONS when it is unset: every class
// away, and project deletion at home too (Jeremy, 2026-10-10).
const DefaultStepUpActions = "admin,budget,provider_key,api_key,export,passkey,training,conversation_delete,project_delete:always"

// DefaultStepUpWindow is how long one passkey assertion counts.
const DefaultStepUpWindow = 10 * time.Minute

type stepUpMode int

const (
	modeAway   stepUpMode = iota + 1 // gated from away
	modeAlways                       // gated at home too
)

// ParseStepUpActions reads WS_STEPUP_ACTIONS: comma-separated class names,
// each optionally "name:away" (the default) or "name:always". "none" turns
// every class off. An unknown class or mode is an error, so a typo cannot
// silently leave something ungated.
func ParseStepUpActions(list string) (map[string]stepUpMode, error) {
	out := map[string]stepUpMode{}
	list = strings.TrimSpace(list)
	if list == "" {
		list = DefaultStepUpActions
	}
	if strings.EqualFold(list, "none") {
		return out, nil
	}
	known := map[string]bool{}
	for _, c := range stepUpClasses {
		known[c] = true
	}
	for _, ent := range strings.Split(list, ",") {
		ent = strings.TrimSpace(ent)
		if ent == "" {
			continue
		}
		name, mode, _ := strings.Cut(ent, ":")
		name = strings.TrimSpace(name)
		if !known[name] {
			return nil, fmt.Errorf("WS_STEPUP_ACTIONS: unknown class %q (classes: %s, or none)", name, strings.Join(stepUpClasses, ", "))
		}
		switch strings.TrimSpace(mode) {
		case "", "away":
			if out[name] != modeAlways {
				out[name] = modeAway
			}
		case "always":
			out[name] = modeAlways
		default:
			return nil, fmt.Errorf("WS_STEPUP_ACTIONS: %q: the mode is away or always", ent)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("WS_STEPUP_ACTIONS: %q names no class (use none to turn step-up off)", list)
	}
	return out, nil
}

// ParseStepUpWindow reads WS_STEPUP_WINDOW (a Go duration: 10m, 90s).
func ParseStepUpWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultStepUpWindow, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("WS_STEPUP_WINDOW: %q is not a duration (for example 10m)", s)
	}
	if d < time.Minute || d > 24*time.Hour {
		return 0, fmt.Errorf("WS_STEPUP_WINDOW: %s is out of range (1m to 24h)", d)
	}
	return d, nil
}

// stepUp is the policy and what it needs to decide.
type stepUp struct {
	modes  map[string]stepUpMode
	window time.Duration
	// hasPasskey reports whether the user can step up at all.
	hasPasskey func(ctx context.Context, userID uuid.UUID) bool
	now        func() time.Time
}

func (s *Server) stepUp() *stepUp {
	su := &stepUp{window: DefaultStepUpWindow, now: time.Now, modes: map[string]stepUpMode{}}
	if s.Cfg != nil {
		if m, err := ParseStepUpActions(s.Cfg.StepUpActions); err == nil { // startup has refused a bad list
			su.modes = m
		}
		if w, err := ParseStepUpWindow(s.Cfg.StepUpWindow); err == nil {
			su.window = w
		}
	}
	su.hasPasskey = func(ctx context.Context, id uuid.UUID) bool { return s.Auth != nil && s.Auth.HasPasskeys(ctx, id) }
	return su
}

// Step-up error codes in the 403 body.
const (
	codeStepUpRequired    = "step_up_required"
	codeStepUpUnavailable = "step_up_unavailable"
)

// check decides one request of a class: "" passes; otherwise the 403 code and message.
func (su *stepUp) check(r *http.Request, class string) (code, msg string) {
	p := Principal(r.Context())
	if p == nil || p.APIKeyID.Valid {
		return "", "" // the auth middleware has already refused, or keys never reach these routes
	}
	loc := LocalityOf(r.Context())
	if !loc.Configured {
		return "", "" // WS_HOME_NETWORKS is unset: inert
	}
	mode, gated := su.modes[class]
	if !gated {
		return "", ""
	}
	away := loc.Locality == Away
	if !away && mode != modeAlways {
		return "", ""
	}
	if !su.hasPasskey(r.Context(), p.UserID) {
		if !away {
			return "", "" // at home, someone with no passkey is unaffected, "always" included
		}
		if class == ActPasskey {
			return codeStepUpUnavailable, "a passkey can only be added or removed from home: no passkey on this account can confirm it from here"
		}
		return codeStepUpUnavailable, "add a passkey to do this away from home"
	}
	if p.SteppedUpAt != nil && su.now().Sub(*p.SteppedUpAt) < su.window {
		return "", ""
	}
	return codeStepUpRequired, "step-up required: confirm with your passkey"
}

// require gates a route of a class.
func (su *stepUp) require(class string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code, msg := su.check(r, class); code != "" {
				writeStepUpErr(w, code, msg, class, su.window)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireAdminWrites gates the writes under /api/admin: budgets are their
// own class, the rest is admin. Reads pass.
func (su *stepUp) requireAdminWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		class := ActAdmin
		if strings.HasPrefix(r.URL.Path, "/api/admin/budgets") {
			class = ActBudget
		}
		su.require(class)(next).ServeHTTP(w, r)
	})
}

// until is when the session's assertion runs out, or the zero time.
func (su *stepUp) until(steppedUpAt *time.Time) time.Time {
	if steppedUpAt == nil {
		return time.Time{}
	}
	if u := steppedUpAt.Add(su.window); u.After(su.now()) {
		return u
	}
	return time.Time{}
}

// writeStepUpErr answers 403 (kept so an older client fails safe) with the
// code the web client looks for.
func writeStepUpErr(w http.ResponseWriter, code, msg, class string, window time.Duration) {
	writeJSON(w, 403, map[string]any{"error": msg, "code": code, "action": class, "window_seconds": int(window.Seconds())})
}

// stepUpBegins limits how often a person may start a step-up ceremony.
type stepUpBegins struct {
	mu   sync.Mutex
	hits map[uuid.UUID][]time.Time
}

func (b *stepUpBegins) allow(user uuid.UUID, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hits == nil {
		b.hits = map[uuid.UUID][]time.Time{}
	}
	cut := now.Add(-time.Minute)
	kept := b.hits[user][:0]
	for _, t := range b.hits[user] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= 10 {
		b.hits[user] = kept
		return false
	}
	b.hits[user] = append(kept, now)
	if len(b.hits) > 5000 { // a brake, not a ledger: forget the idle
		for u, ts := range b.hits {
			if len(ts) == 0 || !ts[len(ts)-1].After(cut) {
				delete(b.hits, u)
			}
		}
	}
	return true
}
