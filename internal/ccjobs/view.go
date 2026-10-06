package ccjobs

import (
	"context"
	"fmt"
	"strings"
)

// View is a read-only tmux client on one job window for the web tab (spec
// §6.5). tmux clients attached to the same session share its current
// window, so attaching the web viewer straight to the subscription session
// would move the Mac's view every time someone opened a job in the
// browser. The view therefore goes through a grouped session (`new-session
// -t subscription`): it shares the windows but has its own current window.
type View struct {
	// Session is the grouped session's name (wsview-<id>).
	Session string
	// Argv is the local command that attaches a terminal to the view
	// read-only (tmux attach -r), through ssh for remote targets. The
	// caller runs it under a pty; nothing of the session passes through
	// any other channel.
	Argv []string
}

const viewPrefix = "wsview-"

// OpenView creates the grouped session for a job window and returns the
// read-only attach command. The window must already exist (use Find).
// Orphaned view sessions from earlier bridges are pruned first.
//
// The session is not created with destroy-unattached: tmux applies that
// option as soon as the creating (detached) client exits, so the view
// would be gone before the bridge attaches. CloseView and PruneViews do
// the cleanup instead.
func (l *Launcher) OpenView(ctx context.Context, w Window) (View, error) {
	if !strings.HasPrefix(w.Window, "@") {
		return View{}, fmt.Errorf("bad window id %q", w.Window)
	}
	_ = l.PruneViews(ctx)
	name := viewPrefix + NewJobID()
	// One command list, so a failed select-window still leaves a session
	// the error path can kill. Inside the list the new session is matched
	// by name (exact "=name" lookups do not see it yet); the name is a
	// fixed-length random id, so no other session is a prefix of it.
	args := []string{
		"new-session", "-d", "-t", "=" + l.Session, "-s", name,
		";", "select-window", "-t", name + ":" + w.Window,
	}
	if _, err := l.Host.Run(ctx, nil, l.tmux(args...)...); err != nil {
		_, _ = l.Host.Run(ctx, nil, l.tmux("kill-session", "-t", "="+name)...)
		return View{}, fmt.Errorf("tmux view session: %w", err)
	}
	argv := l.Host.InteractiveArgv(l.tmux("attach-session", "-r", "-t", "="+name)...)
	return View{Session: name, Argv: argv}, nil
}

// CloseView removes the grouped session. The job window is untouched: it
// belongs to the group, and the subscription session keeps it.
func (l *Launcher) CloseView(ctx context.Context, v View) error {
	if !strings.HasPrefix(v.Session, viewPrefix) {
		return fmt.Errorf("not a view session: %q", v.Session)
	}
	_, err := l.Host.Run(ctx, nil, l.tmux("kill-session", "-t", "="+v.Session)...)
	return err
}

// PruneViews kills view sessions that no client is attached to: bridges
// that died without CloseView. Attached views and every other session are
// left alone. Returns the names removed.
func (l *Launcher) PruneViews(ctx context.Context) []string {
	out, err := l.Host.Run(ctx, nil, l.tmux("list-sessions", "-F", "#{session_name}"+listSep+"#{session_attached}")...)
	if err != nil {
		return nil
	}
	var removed []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		name, attached, ok := strings.Cut(line, listSep)
		if !ok || !strings.HasPrefix(name, viewPrefix) || attached != "0" {
			continue
		}
		if _, err := l.Host.Run(ctx, nil, l.tmux("kill-session", "-t", "="+name)...); err == nil {
			removed = append(removed, name)
		}
	}
	return removed
}
