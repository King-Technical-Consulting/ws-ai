// Package compaction keeps long conversations inside a token budget without
// ever modifying stored messages. It only changes what is sent: older turns
// are replaced by a structured summary block, and large tool results were
// already externalized at write time. Summaries are produced by a cheap
// model via a background job.
package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Block is the structured summary the model produces.
type Block struct {
	Summary     string   `json:"summary"`
	Goals       []string `json:"goals,omitempty"`
	Decisions   []string `json:"decisions,omitempty"`
	OpenThreads []string `json:"open_threads,omitempty"`
	FileState   []string `json:"file_state,omitempty"` // populated from tool records, not model recall
	ToolState   []string `json:"tool_state,omitempty"`
}

// Compactor is both the gateway middleware and the job body.
type Compactor struct {
	DB  *store.DB
	GW  *gateway.Gateway
	Log *slog.Logger
	// BudgetTokens is the soft ceiling for what is sent (default 60k): small
	// enough to fit every model we route to, large enough not to summarize
	// every few turns.
	BudgetTokens int
	// KeepRecent messages are never summarized (default 8).
	KeepRecent int
	// Enqueue schedules a Compact for the conversation (River). nil disables.
	Enqueue func(ctx context.Context, convID uuid.UUID)
}

func (c *Compactor) budget() int {
	if c.BudgetTokens > 0 {
		return c.BudgetTokens
	}
	return 60_000
}

func (c *Compactor) keep() int {
	if c.KeepRecent > 0 {
		return c.KeepRecent
	}
	return 8
}

// EstimateTokens is a cheap pre-flight count (4 chars per token, images
// at a fixed cost). Authoritative counts come back in usage.
func EstimateTokens(req *gateway.Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		n += messageChars(m)
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return n / 4
}

func messageChars(m gateway.Message) int {
	n := 8
	for _, p := range m.Parts {
		n += len(p.Text) + len(p.Args)
		for _, cp := range p.Content {
			n += len(cp.Text)
		}
		if p.Kind == gateway.PartImage {
			n += 6000
		}
	}
	return n
}

// Middleware trims the outgoing request to the budget. Order of operations:
// apply the latest stored summary block in place of the turns it covers;
// if still over budget, drop the oldest remaining turns (keeping tool
// results paired with their calls) and schedule a fresh summary.
func (c *Compactor) Middleware() gateway.Middleware {
	return func(ctx context.Context, req *gateway.Request, in *gateway.RouteInput) error {
		if req.Metadata.External || req.Metadata.ConversationID == "" {
			in.EstTokensIn = EstimateTokens(req)
			return nil
		}
		convID, err := uuid.Parse(req.Metadata.ConversationID)
		if err != nil {
			return nil
		}
		est := EstimateTokens(req)
		if est <= c.budget() {
			in.EstTokensIn = est
			return nil
		}

		// 1. apply stored block
		if comp, err := c.DB.LatestCompaction(ctx, convID); err == nil {
			var blk Block
			_ = json.Unmarshal(comp.Block, &blk)
			req.Messages = applyBlock(req.Messages, comp.CoversThroughSeq, blk)
			est = EstimateTokens(req)
		}
		// 2. still too big: truncate the oldest turns after the block and ask
		//    for a new summary
		if est > c.budget() {
			req.Messages = truncateToBudget(req.Messages, req, c.budget(), c.keep())
			est = EstimateTokens(req)
			if c.Enqueue != nil {
				c.Enqueue(ctx, convID)
			}
			c.Log.Info("compaction: truncated for budget", "conversation", convID, "tokens", est)
		}
		in.EstTokensIn = est
		return nil
	}
}

// applyBlock replaces messages with Seq <= covers by a two-message exchange
// carrying the summary. Messages without a Seq (synthetic) are kept.
func applyBlock(msgs []gateway.Message, covers int64, blk Block) []gateway.Message {
	var out []gateway.Message
	replaced := false
	for _, m := range msgs {
		if m.Seq != 0 && m.Seq <= covers {
			if !replaced {
				replaced = true
				summary := gateway.TextPart(RenderBlock(blk))
				summary.CacheHint = true
				out = append(out,
					gateway.Message{Role: gateway.RoleUser, Parts: []gateway.Part{summary}},
					gateway.Message{Role: gateway.RoleAssistant, Parts: []gateway.Part{gateway.TextPart("Understood. I have the context above and will continue from here.")}},
				)
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

// truncateToBudget drops whole turns from the front until the estimate fits,
// never splitting an assistant tool-call message from the tool results that
// follow it, and never dropping the last `keep` messages.
func truncateToBudget(msgs []gateway.Message, req *gateway.Request, budget, keep int) []gateway.Message {
	fixed := (len(req.System) + toolChars(req)) / 4
	// find how many leading messages we can drop
	total := fixed
	for _, m := range msgs {
		total += messageChars(m) / 4
	}
	i := 0
	for total > budget && i < len(msgs)-keep {
		total -= messageChars(msgs[i]) / 4
		i++
		// don't start the remaining history with orphaned tool results
		for i < len(msgs)-keep && msgs[i].Role == gateway.RoleTool {
			total -= messageChars(msgs[i]) / 4
			i++
		}
	}
	if i == 0 {
		return msgs
	}
	note := gateway.TextPart(fmt.Sprintf("[%d earlier messages omitted for length; a summary is being prepared.]", i))
	return append([]gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{note}}, {Role: gateway.RoleAssistant, Parts: []gateway.Part{gateway.TextPart("Noted.")}}}, msgs[i:]...)
}

func toolChars(req *gateway.Request) int {
	n := 0
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return n
}

// RenderBlock turns a block into the text the model reads.
func RenderBlock(b Block) string {
	var sb strings.Builder
	sb.WriteString("Summary of the conversation so far (earlier turns are not shown):\n\n")
	sb.WriteString(strings.TrimSpace(b.Summary))
	sb.WriteString("\n")
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		sb.WriteString("\n" + title + ":\n")
		for _, it := range items {
			sb.WriteString("- " + it + "\n")
		}
	}
	section("Goals", b.Goals)
	section("Decisions", b.Decisions)
	section("Open threads", b.OpenThreads)
	section("File state", b.FileState)
	section("Tool state", b.ToolState)
	return sb.String()
}

// Compact summarizes everything except the most recent messages into a new
// block and records it. It runs as a background job.
func (c *Compactor) Compact(ctx context.Context, convID uuid.UUID) error {
	rows, err := c.DB.ListMessages(ctx, convID)
	if err != nil {
		return err
	}
	if len(rows) <= c.keep()+2 {
		return nil
	}
	cut := rows[len(rows)-c.keep()-1] // last row to include
	// don't cut between an assistant tool call and its results
	for i := len(rows) - c.keep() - 1; i+1 < len(rows) && rows[i+1].Role == "tool"; i++ {
		cut = rows[i+1]
	}
	prev, _ := c.DB.LatestCompaction(ctx, convID)
	if prev.CoversThroughSeq >= cut.Seq {
		return nil // nothing new to fold in
	}

	var prevBlock Block
	if len(prev.Block) > 0 {
		_ = json.Unmarshal(prev.Block, &prevBlock)
	}
	var transcript strings.Builder
	if prev.CoversThroughSeq > 0 {
		transcript.WriteString("Previous summary:\n" + RenderBlock(prevBlock) + "\n\nNew turns since then:\n\n")
	}
	var fileState []string
	for _, r := range rows {
		if r.Seq <= prev.CoversThroughSeq || r.Seq > cut.Seq {
			continue
		}
		var parts []gateway.Part
		_ = json.Unmarshal(r.Parts, &parts)
		for _, p := range parts {
			switch p.Kind {
			case gateway.PartText:
				fmt.Fprintf(&transcript, "%s: %s\n", r.Role, truncate(p.Text, 4000))
			case gateway.PartToolCall:
				fmt.Fprintf(&transcript, "assistant called %s(%s)\n", p.ToolName, truncate(string(p.Args), 500))
				// file_state from tool records, not recall
				if strings.Contains(p.ToolName, "artifact") || strings.Contains(p.ToolName, "write_file") || strings.Contains(p.ToolName, "edit_file") {
					fileState = append(fileState, fmt.Sprintf("%s %s", p.ToolName, truncate(string(p.Args), 160)))
				}
			case gateway.PartToolResult:
				for _, cp := range p.Content {
					fmt.Fprintf(&transcript, "tool result (%s): %s\n", p.ToolCallID, truncate(cp.Text, 1500))
				}
			}
		}
	}
	if transcript.Len() == 0 {
		return nil
	}

	resp, err := c.GW.Complete(ctx, &gateway.Request{
		Model:  "auto",
		System: `You compress conversation history into a structured JSON record for another model to continue from. Be concrete: keep names, numbers, identifiers, URLs, decisions and anything still unresolved. Omit pleasantries. Reply with JSON only, matching: {"summary": string, "goals": [string], "decisions": [string], "open_threads": [string], "tool_state": [string]}`,
		Messages: []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart(transcript.String())}}},
		MaxTokens: 2000,
		Metadata:  gateway.Metadata{ConversationID: convID.String(), TaskClass: gateway.TaskSummarize},
	})
	if err != nil {
		return fmt.Errorf("compaction: summarize: %w", err)
	}
	var blk Block
	text := strings.TrimSpace(resp.Text())
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		_ = json.Unmarshal([]byte(text[i:j+1]), &blk)
	}
	if blk.Summary == "" {
		blk.Summary = text
	}
	if len(fileState) > 0 {
		blk.FileState = append(prevBlock.FileState, fileState...)
	} else {
		blk.FileState = prevBlock.FileState
	}
	b, _ := json.Marshal(blk)
	if _, err := c.DB.InsertCompaction(ctx, store.InsertCompactionParams{
		ConversationID: convID, CoversThroughSeq: cut.Seq, Block: b, TokenCount: int32(len(RenderBlock(blk)) / 4), EndpointID: strPtr(resp.EndpointID),
	}); err != nil {
		return err
	}
	_ = c.DB.SetCompactionHead(ctx, store.SetCompactionHeadParams{ID: convID, CompactionHeadMessageSeq: &cut.Seq})
	c.Log.Info("compaction: new block", "conversation", convID, "covers_through_seq", cut.Seq, "tokens", len(RenderBlock(blk))/4, "endpoint", resp.EndpointID)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

var _ = time.Now
