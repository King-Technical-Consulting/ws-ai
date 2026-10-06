package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/artifacts"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store/blob"
)

// ---- artifacts ----

// ArtifactTool adapts the artifacts service to the Tool interface.
type ArtifactTool struct {
	Svc  *artifacts.Service
	Name string // create_artifact | update_artifact
}

// ArtifactTools returns both artifact tools.
func ArtifactTools(svc *artifacts.Service) []Tool {
	return []Tool{&ArtifactTool{Svc: svc, Name: "create_artifact"}, &ArtifactTool{Svc: svc, Name: "update_artifact"}}
}

func (t *ArtifactTool) Def() gateway.ToolDef {
	for _, d := range artifacts.ToolDefs() {
		if d.Name == t.Name {
			return d
		}
	}
	return gateway.ToolDef{Name: t.Name}
}
func (t *ArtifactTool) DefaultPolicy() Policy { return PolicyAuto }
func (t *ArtifactTool) Idempotent() bool      { return false } // creates versions
func (t *ArtifactTool) Call(ctx context.Context, tc ToolCtx, args json.RawMessage) (Result, error) {
	ref, err := t.Svc.Call(ctx, tc.ConversationID, tc.MessageID, t.Name, args)
	if err != nil {
		return ErrorResult(err), nil
	}
	return JSONResult(ref), nil
}

// ---- read_blob ----

// ReadBlobTool lets the model page through externalized tool output.
type ReadBlobTool struct{}

func (ReadBlobTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "read_blob",
		Description: "Read part of a large tool output that was stored as a blob. Returns up to 16 KB from the given byte offset.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","description":"The blob key from the stub, e.g. sha256/abc..."},"offset":{"type":"integer","minimum":0,"default":0}},"required":["key"],"additionalProperties":false}`),
	}
}
func (ReadBlobTool) DefaultPolicy() Policy { return PolicyAuto }
func (ReadBlobTool) Idempotent() bool      { return true }
func (ReadBlobTool) Call(ctx context.Context, tc ToolCtx, args json.RawMessage) (Result, error) {
	var in struct {
		Key    string `json:"key"`
		Offset int    `json:"offset"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return ErrorResult(err), nil
	}
	if tc.Blobs == nil {
		return ErrorResult(errors.New("blob store unavailable")), nil
	}
	b, err := blob.GetBytes(ctx, tc.Blobs, in.Key)
	if err != nil {
		return ErrorResult(err), nil
	}
	if in.Offset < 0 || in.Offset >= len(b) {
		return Result{Text: fmt.Sprintf("[end of blob; total %d bytes]", len(b))}, nil
	}
	end := in.Offset + 16*1024
	if end > len(b) {
		end = len(b)
	}
	return Result{Text: fmt.Sprintf("[bytes %d-%d of %d]\n%s", in.Offset, end, len(b), string(b[in.Offset:end]))}, nil
}

// ---- web_fetch ----

// WebFetchTool fetches a public URL and returns its text. It refuses
// private and loopback addresses so a model can't probe the LAN.
type WebFetchTool struct {
	Client *http.Client
}

func (t *WebFetchTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "web_fetch",
		Description: "Fetch a public http(s) URL and return its content as text (HTML is reduced to visible text). Use for documentation pages, articles, and APIs that return text or JSON. Not for private or internal addresses.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"Absolute http or https URL."},"max_chars":{"type":"integer","description":"Cap on returned characters (default 20000).","minimum":1000,"maximum":100000}},"required":["url"],"additionalProperties":false}`),
	}
}
func (t *WebFetchTool) DefaultPolicy() Policy { return PolicyAuto }
func (t *WebFetchTool) Idempotent() bool      { return true }
func (t *WebFetchTool) Call(ctx context.Context, tc ToolCtx, args json.RawMessage) (Result, error) {
	var in struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return ErrorResult(err), nil
	}
	if in.MaxChars <= 0 {
		in.MaxChars = 20000
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrorResult(errors.New("url must be absolute http(s)")), nil
	}
	if err := refusePrivateHost(ctx, u.Hostname()); err != nil {
		return ErrorResult(err), nil
	}
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return refusePrivateHost(req.Context(), req.URL.Hostname())
		}}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
	req.Header.Set("User-Agent", "ws-agent/0.1 (+https://github.com/King-Technical-Consulting/ws)")
	req.Header.Set("Accept", "text/html,application/json,text/plain,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return ErrorResult(err), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return ErrorResult(err), nil
	}
	ct := resp.Header.Get("Content-Type")
	text := string(body)
	if strings.Contains(ct, "text/html") || (ct == "" && strings.Contains(strings.ToLower(text[:min(len(text), 512)]), "<html")) {
		text = htmlToText(text)
	}
	text = strings.TrimSpace(text)
	if len(text) > in.MaxChars {
		text = text[:in.MaxChars] + fmt.Sprintf("\n\n[truncated at %d of %d characters]", in.MaxChars, len(text))
	}
	head := fmt.Sprintf("HTTP %d %s (%s)\n\n", resp.StatusCode, resp.Request.URL, strings.Split(ct, ";")[0])
	return Result{Text: head + text, Data: map[string]any{"status": resp.StatusCode, "url": resp.Request.URL.String(), "content_type": ct, "chars": len(text)}, IsError: resp.StatusCode >= 400}, nil
}

func refusePrivateHost(ctx context.Context, host string) error {
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".home.arpa") || strings.HasSuffix(host, ".internal") {
		return errors.New("refusing to fetch a local or internal address")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() || isCGNAT(ip.IP) {
			return errors.New("refusing to fetch a private or internal address")
		}
	}
	return nil
}

// isCGNAT covers 100.64.0.0/10, which Tailscale uses.
func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// htmlToText strips tags, scripts and styles and collapses whitespace.
// Good enough for docs and articles; not a full extractor.
func htmlToText(s string) string {
	lower := strings.ToLower(s)
	for _, tag := range []string{"script", "style", "noscript", "svg", "head"} {
		for {
			i := strings.Index(lower, "<"+tag)
			if i < 0 {
				break
			}
			j := strings.Index(lower[i:], "</"+tag+">")
			if j < 0 {
				s, lower = s[:i], lower[:i]
				break
			}
			s = s[:i] + " " + s[i+j+len(tag)+3:]
			lower = strings.ToLower(s)
		}
	}
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
			b.WriteByte(' ')
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for _, rep := range [][2]string{{"&nbsp;", " "}, {"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", "\""}, {"&#39;", "'"}} {
		out = strings.ReplaceAll(out, rep[0], rep[1])
	}
	lines := strings.Split(out, "\n")
	var kept []string
	blank := 0
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		kept = append(kept, l)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// ---- ask_user ----

// AskUserTool lets the model pause and ask a clarifying question. The run
// pauses in paused_steer until the user replies.
type AskUserTool struct{}

func (AskUserTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "ask_user",
		Description: "Stop and ask the user a question when you cannot proceed without their answer. Use sparingly; prefer sensible assumptions for small gaps.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"],"additionalProperties":false}`),
	}
}
func (AskUserTool) DefaultPolicy() Policy { return PolicyAuto }
func (AskUserTool) Idempotent() bool      { return true }
func (AskUserTool) Call(ctx context.Context, tc ToolCtx, args json.RawMessage) (Result, error) {
	// The runtime intercepts ask_user before Call; reaching here means the
	// question was answered and the answer is in the next user message.
	return Result{Text: "The user has been asked. Their reply follows as the next message."}, nil
}

var _ = uuid.Nil
