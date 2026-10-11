package chat

import (
	"context"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

func TestCleanTitle(t *testing.T) {
	cases := map[string]string{
		"Deploy lock for ws":                  "Deploy lock for ws",
		"Title: Deploy lock for ws":           "Deploy lock for ws",
		"\"Deploy lock for ws\"":              "Deploy lock for ws",
		"**Deploy lock for ws**.":             "Deploy lock for ws",
		"\n\nDeploy lock for ws\nsecond line": "Deploy lock for ws",
		"title: 'Budget warning mails'":       "Budget warning mails",
		"":                                    "",
		"   \n  ":                             "",
		"\"\"":                                "",
		string(make([]byte, 81)):              "",
	}
	for in, want := range cases {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeTitleGW answers each Complete call with the next scripted response
// and records the budgets it was asked for.
type fakeTitleGW struct {
	replies []*gateway.Response
	budgets []int
}

func (f *fakeTitleGW) Complete(_ context.Context, req *gateway.Request) (*gateway.Response, error) {
	f.budgets = append(f.budgets, req.MaxTokens)
	r := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return r, nil
}

func textResp(text string, finish gateway.FinishReason) *gateway.Response {
	return &gateway.Response{Parts: []gateway.Part{gateway.TextPart(text)}, FinishReason: finish}
}

func TestGenerateTitleRetriesWithMoreRoomWhenAThinkingModelReturnsNothing(t *testing.T) {
	// A reasoning model spent the whole first budget thinking: empty text,
	// finish "length". The second try, with four times the room, answers.
	gw := &fakeTitleGW{replies: []*gateway.Response{
		{FinishReason: gateway.FinishLength},
		textResp("Deploy lock for ws", gateway.FinishStop),
	}}
	title, _, err := generateTitle(context.Background(), gw, "c1", "why does deploy race?", "a lock")
	if err != nil || title != "Deploy lock for ws" {
		t.Fatalf("title %q err %v", title, err)
	}
	if len(gw.budgets) != 2 || gw.budgets[0] != titleTokens || gw.budgets[1] != titleTokens*4 {
		t.Errorf("budgets %v", gw.budgets)
	}
}

func TestGenerateTitleTakesTheFirstGoodAnswerAndGivesUpOnProse(t *testing.T) {
	gw := &fakeTitleGW{replies: []*gateway.Response{textResp("\"Budget warning mails\"", gateway.FinishStop)}}
	title, _, _ := generateTitle(context.Background(), gw, "c1", "q", "a")
	if title != "Budget warning mails" || len(gw.budgets) != 1 {
		t.Errorf("title %q, calls %d", title, len(gw.budgets))
	}
	// A full reply that is not a title (over 80 characters, finish stop)
	// is not retried: more room would not help.
	long := textResp(string(make([]byte, 120)), gateway.FinishStop)
	gw = &fakeTitleGW{replies: []*gateway.Response{long, long}}
	title, last, _ := generateTitle(context.Background(), gw, "c1", "q", "a")
	if title != "" || len(gw.budgets) != 1 || last == nil {
		t.Errorf("title %q, calls %d, last %v", title, len(gw.budgets), last)
	}
}
