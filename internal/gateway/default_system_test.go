package gateway

import (
	"context"
	"sync"
	"testing"
)

// sysAdapter records the system prompt each request reached the provider with.
type sysAdapter struct {
	mu   sync.Mutex
	seen []string
}

func (a *sysAdapter) Stream(_ context.Context, _ *Provider, ep *Endpoint, req *Request) (<-chan StreamEvent, error) {
	a.mu.Lock()
	a.seen = append(a.seen, req.System)
	a.mu.Unlock()
	ch := make(chan StreamEvent, 4)
	ch <- StreamEvent{Type: EventStart, EndpointID: ep.ID, Model: ep.ModelName}
	ch <- StreamEvent{Type: EventTextDelta, Text: "ok"}
	ch <- StreamEvent{Type: EventFinish, FinishReason: FinishStop}
	close(ch)
	return ch, nil
}
func (*sysAdapter) CountTokens(context.Context, *Provider, *Endpoint, *Request) (int, bool, error) {
	return 0, false, nil
}

// An endpoint with a default system prompt (an adapter trained from a
// recipe) serves under it when the request brings none, and the request's
// own system prompt wins; the caller's request is never changed.
func TestEndpointDefaultSystem(t *testing.T) {
	p, _ := ParsePolicy(testPolicy)
	reg := testRegistry()
	ep, ok := reg.Endpoint("local/small")
	if !ok {
		t.Fatal("no local/small in the test registry")
	}
	ep.DefaultSystem = "You answer about whales."
	g := New(reg, NewRouter(reg, []Policy{p}), nil, nil)
	sa := &sysAdapter{}
	g.RegisterAdapter(ProviderAnthropic, sa)
	g.RegisterAdapter(ProviderOpenAICompat, sa)

	bare := &Request{Model: "local/small", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("x")}}}}
	if _, err := g.Complete(context.Background(), bare); err != nil {
		t.Fatal(err)
	}
	own := &Request{Model: "local/small", System: "Be terse.", Messages: bare.Messages}
	if _, err := g.Complete(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	if len(sa.seen) != 2 || sa.seen[0] != "You answer about whales." || sa.seen[1] != "Be terse." {
		t.Errorf("system prompts that reached the provider = %q", sa.seen)
	}
	if bare.System != "" {
		t.Errorf("the caller's request was changed: %q", bare.System)
	}
}

func TestLiftDefaultSystem(t *testing.T) {
	e := &Endpoint{ExtraBody: map[string]any{DefaultSystemKey: "S", "lora": 1}}
	e.LiftDefaultSystem()
	if e.DefaultSystem != "S" || len(e.ExtraBody) != 1 || e.ExtraBody["lora"] != 1 {
		t.Errorf("endpoint = %+v", e)
	}
	none := &Endpoint{}
	none.LiftDefaultSystem() // no extra body at all must not panic
	if none.DefaultSystem != "" {
		t.Errorf("endpoint = %+v", none)
	}
}
