package ccweb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"

	"github.com/jking323/ws/internal/ccjobs"
	"github.com/jking323/ws/internal/store"
)

// Terminal is one browser tab's read-only view of a job window: a pty
// running `tmux attach -r` (through ssh for remote targets) on a grouped
// session of its own. Bytes flow one way, pty to reader; the only thing
// written back is the terminal size. Nothing is buffered beyond the read
// in flight and nothing is logged.
type Terminal struct {
	Job  store.CcJob
	f    *os.File
	cmd  *exec.Cmd
	l    *ccjobs.Launcher
	view ccjobs.View
}

// Open starts a terminal on a job. The window is looked up on the target
// first: a job whose window is gone is marked so and ErrGone returned.
func (r *Registry) Open(ctx context.Context, jobID string, cols, rows int) (*Terminal, error) {
	if !ccjobs.ValidJobID(jobID) {
		return nil, fmt.Errorf("bad job id %q", jobID)
	}
	job, err := r.DB.GetCCJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if r.Targets == nil {
		return nil, errors.New("no launcher targets on this host: the terminal cannot reach the job")
	}
	cfg, err := r.Targets()
	if err != nil {
		return nil, fmt.Errorf("launcher targets: %w", err)
	}
	l, err := cfg.Launcher(job.Target)
	if err != nil {
		return nil, err
	}
	fctx, cancel := context.WithTimeout(ctx, r.targetTimeout())
	defer cancel()
	w, err := l.Find(fctx, job.ID)
	if err != nil {
		var re *ccjobs.RunError
		if errors.As(err, &re) {
			return nil, err // the target itself failed; say nothing about the job
		}
		_ = r.DB.MarkCCJobEnded(ctx, store.MarkCCJobEndedParams{ID: job.ID, Status: StatusGone})
		return nil, ErrGone
	}
	status := StatusAlive
	if w.Dead {
		status = StatusDead
	}
	_ = r.DB.MarkCCJobSeen(ctx, store.MarkCCJobSeenParams{ID: job.ID, Status: status})

	view, err := l.OpenView(fctx, w)
	if err != nil {
		return nil, err
	}
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 32
	}
	cmd := exec.Command(view.Argv[0], view.Argv[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		_ = l.CloseView(context.WithoutCancel(ctx), view)
		return nil, fmt.Errorf("start terminal: %w", err)
	}
	return &Terminal{Job: job, f: f, cmd: cmd, l: l, view: view}, nil
}

// Read returns the next terminal bytes. io.EOF (or an EIO, which a pty
// reports when its process exits) ends the view.
func (t *Terminal) Read(p []byte) (int, error) { return t.f.Read(p) }

// Resize tells tmux the browser's new size.
func (t *Terminal) Resize(cols, rows int) {
	if cols > 0 && rows > 0 && cols < 1000 && rows < 1000 {
		_ = pty.Setsize(t.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
}

// Close ends the attach, releases the pty and removes the view session.
func (t *Terminal) Close() error {
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.f.Close()
	_ = t.cmd.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return t.l.CloseView(ctx, t.view)
}
