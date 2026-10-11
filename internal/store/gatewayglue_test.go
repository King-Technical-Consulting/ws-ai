package store

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestNoServerKeyIsLoggedOnceAndWhenTheSetChanges(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	noKeyMu.Lock()
	noKeySeen, noKeyLast = false, ""
	noKeyMu.Unlock()

	logNoServerKey([]string{"fal (FAL_KEY)", "anthropic (ANTHROPIC_API_KEY)"})
	logNoServerKey([]string{"anthropic (ANTHROPIC_API_KEY)", "fal (FAL_KEY)"}) // the same set, any order
	logNoServerKey([]string{"anthropic (ANTHROPIC_API_KEY)", "fal (FAL_KEY)"})
	if n := strings.Count(buf.String(), "without a server key"); n != 1 {
		t.Fatalf("logged %d times for an unchanged set:\n%s", n, buf.String())
	}
	logNoServerKey([]string{"fal (FAL_KEY)"})
	if n := strings.Count(buf.String(), "without a server key"); n != 2 {
		t.Errorf("a changed set should log again; %d lines", n)
	}
	logNoServerKey(nil)
	if !strings.Contains(buf.String(), "every hosted provider has a server key") {
		t.Error("the set emptying should be said")
	}
}
