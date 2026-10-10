package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

func TestUserFacing(t *testing.T) {
	noRoute := fmt.Errorf("%w: router: endpoint local-llama/gpt-oss-20b unavailable: context too small", gateway.ErrNoRoute)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"router refused for size", noRoute, tooLongMsg},
		{"server refused for size", errors.New("openaicompat: HTTP 400: request (63207 tokens) exceeds the available context size (32768 tokens), try increasing it"), tooLongMsg},
		{"no route for another reason", fmt.Errorf("%w: router: endpoint x unavailable: down", gateway.ErrNoRoute), "No model is available for this request right now."},
		{"hosted model without a key", fmt.Errorf("%w: router: no endpoint satisfies selector \"auto\": needs your own API key", gateway.ErrNoRoute), needsKeyMsg},
		{"direct pick without a key", fmt.Errorf("%w: router: endpoint anthropic/haiku unavailable: no API key of yours for anthropic", gateway.ErrNoRoute), needsKeyMsg},
		{"image on a text-only alias", fmt.Errorf("%w: router: no endpoint satisfies selector \"local\" task \"chat\" (caps {Vision:true}, downgrade=false): needs a model that can read images", gateway.ErrNoRoute), noVisionMsg},
		{"image where the vision models need a key", fmt.Errorf("%w: router: no endpoint satisfies selector \"auto\": needs a model that can read images: needs your own API key", gateway.ErrNoRoute), noVisionNeedsKeyMsg},
		{"image on a direct text-only pick", fmt.Errorf("%w: router: endpoint local/small unavailable: no vision", gateway.ErrNoRoute), noVisionMsg},
		{"no image engine configured", fmt.Errorf("%w: %w", gateway.ErrNoRoute, fmt.Errorf("router: no endpoint satisfies selector \"auto\": %w", &gateway.NoMediaError{Kind: "image"})), "No image endpoint is configured; the owner sets COMFYUI_URL or a media provider key, or enables one under Admin, endpoints."},
		{"image engines all disabled", fmt.Errorf("%w: %w", gateway.ErrNoRoute, &gateway.NoMediaError{Kind: "video", Disabled: true}), "Every video endpoint is disabled; the owner enables one under Admin, endpoints."},
		{"canceled", context.Canceled, "Stopped."},
	}
	for _, c := range cases {
		if got := userFacing(c.err); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := userFacing(noRoute); strings.Contains(got, "llama") || strings.Contains(got, "try increasing") {
		t.Errorf("message leaks server detail: %q", got)
	}
}
