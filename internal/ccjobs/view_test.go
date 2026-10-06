package ccjobs

import (
	"context"
	"strings"
	"testing"
	"time"
)

// sessions returns name -> "group|grouped|current window id" for the test server.
func sessions(t *testing.T, l *Launcher) map[string][3]string {
	t.Helper()
	out, err := l.Host.Run(context.Background(), nil, l.tmux("list-sessions", "-F", "#{session_name}|~|#{session_group}|~|#{session_grouped}|~|#{window_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string][3]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|~|")
		if len(f) == 4 {
			m[f[0]] = [3]string{f[1], f[2], f[3]}
		}
	}
	return m
}

// TestOpenCloseView: the web viewer gets a grouped session on the job
// window with its own current window, attached read-only; the group goes
// away on close or when pruned, and the job windows stay.
func TestOpenCloseView(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	st := writeStub(t, tmp, "sleep 60")
	l := testLauncher(t, LocalHost{name: "test"}, st.path)
	l.DefaultDir = tmp
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h1, err := l.Launch(ctx, Job{Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Launch(ctx, Job{Prompt: "two"}); err != nil {
		t.Fatal(err)
	}
	w1, err := l.Find(ctx, h1.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := l.OpenView(ctx, w1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v.Session, "wsview-") {
		t.Errorf("view session = %q", v.Session)
	}
	if got := strings.Join(v.Argv, " "); got != "tmux -L "+l.TmuxSocket+" attach-session -r -t ="+v.Session {
		t.Errorf("view argv = %q (must be read-only)", got)
	}
	ss := sessions(t, l)
	view, ok := ss[v.Session]
	if !ok || view[1] != "1" {
		t.Fatalf("view session missing or not grouped: %v", ss)
	}
	if sub := ss[l.Session]; sub[0] != view[0] {
		t.Errorf("job session group %q, view group %q", sub[0], view[0])
	}
	// The view's current window is the job's; the job session's did not move.
	if view[2] != w1.Window {
		t.Errorf("view current window = %q want %q", view[2], w1.Window)
	}
	if ss[l.Session][2] == w1.Window {
		t.Errorf("job session's current window moved to the viewed job")
	}

	if err := l.CloseView(ctx, v); err != nil {
		t.Fatal(err)
	}
	if _, still := sessions(t, l)[v.Session]; still {
		t.Errorf("view session still present after close")
	}
	wins, err := l.List(ctx)
	if err != nil || len(wins) != 2 {
		t.Fatalf("job windows after close = %+v, %v", wins, err)
	}
	if err := l.CloseView(ctx, View{Session: l.Session}); err == nil {
		t.Error("CloseView must refuse a session that is not a view")
	}

	// A view nobody closed is pruned by the next open; the job session is not.
	leak, err := l.OpenView(ctx, w1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := l.OpenView(ctx, w1)
	if err != nil {
		t.Fatal(err)
	}
	ss = sessions(t, l)
	if _, still := ss[leak.Session]; still {
		t.Errorf("unattached view %s not pruned", leak.Session)
	}
	if _, ok := ss[v2.Session]; !ok {
		t.Errorf("fresh view %s missing", v2.Session)
	}
	if _, ok := ss[l.Session]; !ok {
		t.Errorf("job session pruned")
	}
	_ = l.CloseView(ctx, v2)
}
