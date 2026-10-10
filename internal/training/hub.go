package training

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Hub searches the Hugging Face model hub for base models the trainer can
// load. The browser never calls the hub itself: the server proxies the
// search (so HF_TOKEN, when set, can see gated and private models, and
// so the page works on a box whose browser has no route out), keeps a
// short cache per query, and drops what the trainer cannot use (GGUF-only
// repositories, non-transformers libraries, models that are not text
// generation).
type Hub struct {
	Client *http.Client
	Token  string
	// Base is the hub's API root; empty means https://huggingface.co.
	Base string
	// TTL bounds the cache per query; empty means ten minutes.
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]hubEntry
}

type hubEntry struct {
	at     time.Time
	models []HubModel
}

// HubModel is one search hit as the page shows it.
type HubModel struct {
	ID        string `json:"id"`
	Downloads int64  `json:"downloads"`
	Likes     int64  `json:"likes"`
	// Params is the parameter count from the safetensors metadata when
	// the hub has it, so the list can say "0.5B".
	Params int64 `json:"params,omitempty"`
	// Gated is true when accepting a license on the hub (and HF_TOKEN on
	// the trainer) is needed to download it.
	Gated bool `json:"gated"`
}

// Suggested is what an empty query returns: small instruct bases that
// train on an Orin-class box and are not gated, newest first.
var Suggested = []HubModel{
	{ID: "Qwen/Qwen2.5-0.5B-Instruct", Params: 494_000_000},
	{ID: "Qwen/Qwen2.5-1.5B-Instruct", Params: 1_540_000_000},
	{ID: "Qwen/Qwen2.5-3B-Instruct", Params: 3_090_000_000},
	{ID: "Qwen/Qwen2.5-7B-Instruct", Params: 7_620_000_000},
	{ID: "Qwen/Qwen3-0.6B", Params: 600_000_000},
	{ID: "Qwen/Qwen3-1.7B", Params: 1_700_000_000},
	{ID: "Qwen/Qwen3-4B", Params: 4_000_000_000},
	{ID: "HuggingFaceTB/SmolLM2-1.7B-Instruct", Params: 1_710_000_000},
	{ID: "google/gemma-3-1b-it", Params: 1_000_000_000, Gated: true},
	{ID: "meta-llama/Llama-3.2-1B-Instruct", Params: 1_240_000_000, Gated: true},
	{ID: "meta-llama/Llama-3.2-3B-Instruct", Params: 3_210_000_000, Gated: true},
}

const hubLimit = 20

// Search returns up to hubLimit models matching q, most downloaded first.
// An empty query returns Suggested. Errors from the hub are returned as
// is; the handler turns them into a 502 so the page can say the hub was
// unreachable rather than show an empty list.
func (h *Hub) Search(ctx context.Context, q string) ([]HubModel, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return Suggested, nil
	}
	if len(q) > 100 {
		q = q[:100]
	}
	key := strings.ToLower(q)
	ttl := h.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	h.mu.Lock()
	if e, ok := h.cache[key]; ok && time.Since(e.at) < ttl {
		h.mu.Unlock()
		return e.models, nil
	}
	h.mu.Unlock()

	models, err := h.fetch(ctx, q, true)
	if err != nil {
		// The expand fields are documented but the hub has refused
		// combinations before; the plain listing carries everything but
		// the parameter count and the gated flag.
		models, err = h.fetch(ctx, q, false)
		if err != nil {
			return nil, err
		}
	}
	h.mu.Lock()
	if h.cache == nil {
		h.cache = map[string]hubEntry{}
	}
	h.cache[key] = hubEntry{at: time.Now(), models: models}
	h.mu.Unlock()
	return models, nil
}

func (h *Hub) fetch(ctx context.Context, q string, expand bool) ([]HubModel, error) {
	base := h.Base
	if base == "" {
		base = "https://huggingface.co"
	}
	v := url.Values{}
	v.Set("search", q)
	v.Set("filter", "text-generation")
	v.Set("sort", "downloads")
	v.Set("direction", "-1")
	v.Set("limit", fmt.Sprint(hubLimit*2)) // room for the ones filtered out
	if expand {
		for _, f := range []string{"downloads", "likes", "gated", "library_name", "tags", "safetensors"} {
			v.Add("expand[]", f)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/models?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ws-trainer-hub-search")
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("hub: %s", strings.TrimSpace(http.StatusText(res.StatusCode)+" "+string(truncateBytes(body, 200))))
	}
	var raw []struct {
		ID          string          `json:"id"`
		ModelID     string          `json:"modelId"`
		Downloads   int64           `json:"downloads"`
		Likes       int64           `json:"likes"`
		Private     bool            `json:"private"`
		Gated       json.RawMessage `json:"gated"`
		Library     string          `json:"library_name"`
		Tags        []string        `json:"tags"`
		Pipeline    string          `json:"pipeline_tag"`
		Safetensors *struct {
			Total int64 `json:"total"`
		} `json:"safetensors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("hub: %w", err)
	}
	out := make([]HubModel, 0, len(raw))
	for _, r := range raw {
		id := r.ID
		if id == "" {
			id = r.ModelID
		}
		if id == "" || !usableOnTrainer(r.Library, r.Tags) {
			continue
		}
		m := HubModel{ID: id, Downloads: r.Downloads, Likes: r.Likes, Gated: gatedFlag(r.Gated)}
		if r.Safetensors != nil {
			m.Params = r.Safetensors.Total
		}
		out = append(out, m)
		if len(out) == hubLimit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Downloads > out[j].Downloads })
	return out, nil
}

// usableOnTrainer keeps what train.py can load with transformers: a
// transformers repository (the library when the hub says it, else the
// tag) that is not a GGUF or MLX conversion.
func usableOnTrainer(library string, tags []string) bool {
	has := func(t string) bool {
		for _, x := range tags {
			if x == t {
				return true
			}
		}
		return false
	}
	if has("gguf") || has("mlx") {
		return false
	}
	if library != "" {
		return library == "transformers"
	}
	return has("transformers") || len(tags) == 0
}

// gatedFlag reads the hub's gated field, which is false, or a string such
// as "auto" or "manual" when a license must be accepted.
func gatedFlag(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "false" && s != "null"
}

func truncateBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
