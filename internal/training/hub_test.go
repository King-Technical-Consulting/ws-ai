package training

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHubSearchFiltersAndCaches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/models" || r.URL.Query().Get("search") != "qwen" || r.URL.Query().Get("filter") != "text-generation" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("token not sent: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		// One hit with every field, one GGUF conversion, one gated model
		// without a safetensors block, one from another library.
		_, _ = w.Write([]byte(`[
		  {"id":"Qwen/Qwen2.5-0.5B-Instruct","downloads":100,"likes":5,"gated":false,"library_name":"transformers","tags":["transformers","safetensors"],"safetensors":{"total":494032768}},
		  {"id":"bartowski/Qwen2.5-0.5B-GGUF","downloads":900,"likes":1,"gated":false,"library_name":"transformers","tags":["gguf"]},
		  {"id":"meta-llama/Llama-3.2-1B","downloads":500,"likes":9,"gated":"manual","library_name":"transformers","tags":["transformers"]},
		  {"id":"mlx-community/x","downloads":50,"library_name":"mlx","tags":["mlx"]}
		]`))
	}))
	defer srv.Close()
	h := &Hub{Base: srv.URL, Token: "tok", TTL: time.Hour}
	got, err := h.Search(context.Background(), " qwen ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "meta-llama/Llama-3.2-1B" || !got[0].Gated || got[1].ID != "Qwen/Qwen2.5-0.5B-Instruct" || got[1].Params != 494032768 || got[1].Gated {
		t.Errorf("got %+v", got)
	}
	if _, err := h.Search(context.Background(), "QWEN"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("cache miss: %d calls", calls)
	}
	// An empty query is the suggested list, never a hub call.
	if s, _ := h.Search(context.Background(), ""); len(s) == 0 || s[0].ID != Suggested[0].ID || calls != 1 {
		t.Errorf("suggested: %+v calls=%d", s, calls)
	}
}

func TestHubSearchFallsBackWithoutExpandAndReportsErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if len(r.URL.Query()["expand[]"]) > 0 {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"modelId":"org/plain","downloads":3,"tags":[]}]`))
	}))
	defer srv.Close()
	h := &Hub{Base: srv.URL}
	got, err := h.Search(context.Background(), "plain")
	if err != nil || len(got) != 1 || got[0].ID != "org/plain" || calls != 2 {
		t.Errorf("got %+v err=%v calls=%d", got, err, calls)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer down.Close()
	if _, err := (&Hub{Base: down.URL}).Search(context.Background(), "x"); err == nil {
		t.Error("expected an error from a 503")
	}
}
