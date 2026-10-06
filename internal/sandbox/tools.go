package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
)

// Resolver finds (or creates) the sandbox a tool call should run in. It is
// wired by the worker: conversation → project → Manager.Ensure. It returns
// an error for conversations outside code projects.
type Resolver func(ctx context.Context, tc agent.ToolCtx) (*Sandbox, error)

// Tools returns the sandbox tool set.
func Tools(m *Manager, resolve Resolver) []agent.Tool {
	return []agent.Tool{
		&readFileTool{m, resolve}, &writeFileTool{m, resolve}, &editFileTool{m, resolve},
		&listFilesTool{m, resolve}, &grepTool{m, resolve}, &bashTool{m, resolve},
	}
}

// ToolNames lists the sandbox tools, for ToolAllow lists.
var ToolNames = []string{"read_file", "write_file", "edit_file", "list_files", "grep", "bash"}

// resolvePath confines a user-supplied path to the workspace.
func resolvePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return Workspace, nil
	}
	if !strings.HasPrefix(p, "/") {
		p = path.Join(Workspace, p)
	}
	p = path.Clean(p)
	if p != Workspace && !strings.HasPrefix(p, Workspace+"/") {
		return "", fmt.Errorf("path %q is outside %s", p, Workspace)
	}
	return p, nil
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func textResult(s string) (agent.Result, error) { return agent.Result{Text: s}, nil }
func errResult(err error) (agent.Result, error) {
	return agent.Result{Text: err.Error(), IsError: true}, nil
}

// ---- read_file ----

type readFileTool struct {
	m       *Manager
	resolve Resolver
}

func (t *readFileTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "read_file",
		Description: "Read a file from the project workspace. Returns the content with line numbers. Use offset/limit for large files.",
		InputSchema: schema(`{"type":"object","properties":{"path":{"type":"string","description":"Path, relative to the workspace root or absolute under /workspace"},"offset":{"type":"integer","description":"1-based first line to return","minimum":1},"limit":{"type":"integer","description":"Max lines to return (default 2000)","minimum":1}},"required":["path"]}`),
	}
}
func (t *readFileTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *readFileTool) Idempotent() bool            { return true }
func (t *readFileTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return errResult(err)
	}
	p, err := resolvePath(in.Path)
	if err != nil {
		return errResult(err)
	}
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	data, err := t.m.ReadFile(ctx, sb, p, 4<<20)
	if err != nil {
		return errResult(err)
	}
	if isBinary(data) {
		return textResult(fmt.Sprintf("%s: binary file, %d bytes", p, len(data)))
	}
	lines := strings.Split(string(data), "\n")
	if in.Offset < 1 {
		in.Offset = 1
	}
	if in.Limit < 1 {
		in.Limit = 2000
	}
	if in.Offset > len(lines) {
		return textResult(fmt.Sprintf("%s has %d lines; offset %d is past the end", p, len(lines), in.Offset))
	}
	end := min(len(lines), in.Offset-1+in.Limit)
	var b strings.Builder
	for i := in.Offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "[%d more lines; use offset=%d]\n", len(lines)-end, end+1)
	}
	return textResult(b.String())
}

func isBinary(b []byte) bool {
	n := min(len(b), 8000)
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}

// ---- write_file ----

type writeFileTool struct {
	m       *Manager
	resolve Resolver
}

func (t *writeFileTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "write_file",
		Description: "Create or overwrite a file in the workspace with the given content. Parent directories are created. Prefer edit_file for small changes to existing files.",
		InputSchema: schema(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
	}
}
func (t *writeFileTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *writeFileTool) Idempotent() bool            { return true }
func (t *writeFileTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return errResult(err)
	}
	p, err := resolvePath(in.Path)
	if err != nil {
		return errResult(err)
	}
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	if err := t.m.WriteFile(ctx, sb, p, []byte(in.Content)); err != nil {
		return errResult(err)
	}
	return agent.Result{Text: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), p), Data: map[string]any{"path": p, "bytes": len(in.Content)}}, nil
}

// ---- edit_file ----

type editFileTool struct {
	m       *Manager
	resolve Resolver
}

func (t *editFileTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "edit_file",
		Description: "Replace an exact string in a file. old_string must match exactly once unless replace_all is true. Include enough surrounding lines to make it unique.",
		InputSchema: schema(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_string","new_string"]}`),
	}
}
func (t *editFileTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *editFileTool) Idempotent() bool            { return true }
func (t *editFileTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return errResult(err)
	}
	if in.Old == "" {
		return errResult(errors.New("old_string must not be empty"))
	}
	p, err := resolvePath(in.Path)
	if err != nil {
		return errResult(err)
	}
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	data, err := t.m.ReadFile(ctx, sb, p, 4<<20)
	if err != nil {
		return errResult(err)
	}
	s := string(data)
	n := strings.Count(s, in.Old)
	switch {
	case n == 0:
		return errResult(fmt.Errorf("old_string not found in %s", p))
	case n > 1 && !in.ReplaceAll:
		return errResult(fmt.Errorf("old_string matches %d times in %s; make it unique or set replace_all", n, p))
	}
	if in.ReplaceAll {
		s = strings.ReplaceAll(s, in.Old, in.New)
	} else {
		s = strings.Replace(s, in.Old, in.New, 1)
	}
	if err := t.m.WriteFile(ctx, sb, p, []byte(s)); err != nil {
		return errResult(err)
	}
	return agent.Result{Text: fmt.Sprintf("edited %s (%d replacement(s))", p, n), Data: map[string]any{"path": p, "replacements": n}}, nil
}

// ---- list_files ----

type listFilesTool struct {
	m       *Manager
	resolve Resolver
}

func (t *listFilesTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "list_files",
		Description: "List files under a directory in the workspace, honoring .gitignore. Optional glob filters by name, e.g. \"*.go\" or \"src/**/*.ts\". Returns up to 500 paths.",
		InputSchema: schema(`{"type":"object","properties":{"path":{"type":"string","description":"Directory (default workspace root)"},"glob":{"type":"string"}}}`),
	}
}
func (t *listFilesTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *listFilesTool) Idempotent() bool            { return true }
func (t *listFilesTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Path string `json:"path"`
		Glob string `json:"glob"`
	}
	_ = json.Unmarshal(args, &in)
	p, err := resolvePath(in.Path)
	if err != nil {
		return errResult(err)
	}
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	cmd := []string{"rg", "--files", "--sort", "path", "--hidden", "-g", "!.git"}
	if in.Glob != "" {
		cmd = append(cmd, "-g", in.Glob)
	}
	cmd = append(cmd, p)
	res, err := t.m.Exec(ctx, sb, ExecOptions{Cmd: cmd, Timeout: 30 * time.Second, MaxOutput: 256 << 10})
	if err != nil {
		return errResult(err)
	}
	if res.ExitCode == 2 {
		return errResult(errors.New(strings.TrimSpace(res.Stderr)))
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return textResult("no files")
	}
	more := ""
	if len(lines) > 500 {
		more = fmt.Sprintf("\n[%d more; narrow with path or glob]", len(lines)-500)
		lines = lines[:500]
	}
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimPrefix(l, Workspace), "/")
	}
	return textResult(strings.Join(lines, "\n") + more)
}

// ---- grep ----

type grepTool struct {
	m       *Manager
	resolve Resolver
}

func (t *grepTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "grep",
		Description: "Search file contents with ripgrep (regex, smart case). Returns path:line:text, at most 200 matches.",
		InputSchema: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"File or directory (default workspace root)"},"glob":{"type":"string","description":"Only files matching this glob"},"context":{"type":"integer","description":"Lines of context around each match"}},"required":["pattern"]}`),
	}
}
func (t *grepTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *grepTool) Idempotent() bool            { return true }
func (t *grepTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
		Context int    `json:"context"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return errResult(err)
	}
	if in.Pattern == "" {
		return errResult(errors.New("pattern is required"))
	}
	p, err := resolvePath(in.Path)
	if err != nil {
		return errResult(err)
	}
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	cmd := []string{"rg", "-n", "--no-heading", "-S", "--hidden", "-g", "!.git", "-m", "200", "--max-columns", "400"}
	if in.Glob != "" {
		cmd = append(cmd, "-g", in.Glob)
	}
	if in.Context > 0 {
		cmd = append(cmd, "-C", strconv.Itoa(min(in.Context, 10)))
	}
	cmd = append(cmd, "-e", in.Pattern, p)
	res, err := t.m.Exec(ctx, sb, ExecOptions{Cmd: cmd, Timeout: 30 * time.Second, MaxOutput: 256 << 10})
	if err != nil {
		return errResult(err)
	}
	switch res.ExitCode {
	case 0:
		out := strings.ReplaceAll(res.Stdout, Workspace+"/", "")
		return textResult(out)
	case 1:
		return textResult("no matches")
	default:
		return errResult(errors.New(strings.TrimSpace(res.Stderr)))
	}
}

// ---- bash ----

type bashTool struct {
	m       *Manager
	resolve Resolver
}

func (t *bashTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "bash",
		Description: "Run a shell command in the project workspace (bash, non-interactive, no TTY). Network access is limited to package registries and GitHub through a proxy. Default timeout 120 s, max 600 s. Returns stdout, stderr and the exit code.",
		InputSchema: schema(`{"type":"object","properties":{"command":{"type":"string"},"timeout_s":{"type":"integer","minimum":1,"maximum":600},"cwd":{"type":"string","description":"Working directory under /workspace"}},"required":["command"]}`),
	}
}
func (t *bashTool) DefaultPolicy() agent.Policy { return agent.PolicyAsk }
func (t *bashTool) Idempotent() bool            { return false }
func (t *bashTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Command  string `json:"command"`
		TimeoutS int    `json:"timeout_s"`
		Cwd      string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return errResult(err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return errResult(errors.New("command is required"))
	}
	dir, err := resolvePath(in.Cwd)
	if err != nil {
		return errResult(err)
	}
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	timeout := 120 * time.Second
	if in.TimeoutS > 0 {
		timeout = time.Duration(min(in.TimeoutS, 600)) * time.Second
	}
	if tc.Emit != nil {
		tc.Emit("sandbox-exec", map[string]any{"command": in.Command, "cwd": dir})
	}
	res, err := t.m.Exec(ctx, sb, ExecOptions{Shell: in.Command, Dir: dir, Timeout: timeout, MaxOutput: 512 << 10})
	if err != nil {
		return errResult(err)
	}
	var b strings.Builder
	if res.Stdout != "" {
		b.WriteString(res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			b.WriteString("\n")
		}
	}
	if res.Stderr != "" {
		b.WriteString("[stderr]\n")
		b.WriteString(res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			b.WriteString("\n")
		}
	}
	switch {
	case res.TimedOut:
		fmt.Fprintf(&b, "[killed after %s]\n", timeout)
	case res.ExitCode != 0:
		fmt.Fprintf(&b, "[exit code %d]\n", res.ExitCode)
	}
	if b.Len() == 0 {
		b.WriteString("(no output)\n")
	}
	return agent.Result{
		Text:    b.String(),
		Data:    map[string]any{"exit_code": res.ExitCode, "timed_out": res.TimedOut, "duration_ms": res.Duration.Milliseconds(), "truncated": res.Truncated},
		IsError: res.TimedOut || res.ExitCode != 0,
	}, nil
}
