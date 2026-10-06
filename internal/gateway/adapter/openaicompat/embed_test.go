package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

func TestEmbedAgainstServer(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "wrong path " + r.URL.Path}})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		inputs := got["input"].([]any)
		data := []map[string]any{}
		for i := range inputs {
			// Out of order on purpose: the adapter must place by index.
			data = append([]map[string]any{{"object": "embedding", "index": i, "embedding": []float64{float64(i), 0.5}}}, data...)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": "m", "data": data, "usage": map[string]int{"prompt_tokens": 9, "total_tokens": 9}})
	}))
	defer srv.Close()
	a := New()
	p := &gateway.Provider{ID: "local", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL + "/v1"}
	ep := &gateway.Endpoint{ID: "local/m", ModelName: "m", ExtraBody: map[string]any{"dimensions": 768}}
	resp, err := a.Embed(context.Background(), p, ep, &gateway.EmbedRequest{Inputs: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "m" || got["dimensions"] != float64(768) {
		t.Errorf("request = %v", got)
	}
	if len(resp.Vectors) != 2 || resp.Vectors[0][0] != 0 || resp.Vectors[1][0] != 1 || resp.Usage.InputTokens != 9 || resp.Model != "m" {
		t.Errorf("resp = %+v", resp)
	}
}
