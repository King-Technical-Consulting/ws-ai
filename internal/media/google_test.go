package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jking323/ws/internal/gateway"
)

func TestGoogleEngine(t *testing.T) {
	var mu sync.Mutex
	var gotKey, gotPath string
	var gotBody map[string]any
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotKey = r.Header.Get("x-goog-api-key")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":predict"):
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"predictions": []map[string]any{
				{"bytesBase64Encoded": base64Encode(pngBytes(5, 5)), "mimeType": "image/png"},
				{"raiFilteredReason": "safety"},
			}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":predictLongRunning"):
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "models/veo-3.0-generate-001/operations/op-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/models/veo-3.0-generate-001/operations/op-1":
			polls++
			if polls < 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "models/veo-3.0-generate-001/operations/op-1", "done": false})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "models/veo-3.0-generate-001/operations/op-1", "done": true, "response": map[string]any{
				"generateVideoResponse": map[string]any{"generatedSamples": []map[string]any{{"video": map[string]any{"uri": "http://" + r.Host + "/files/v1:download?alt=media"}}}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/files/v1:download":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("mp4"))
		default:
			http.Error(w, "wrong route "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	p := &gateway.Provider{ID: "google", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL, APIKey: "g-key"}
	eng := &Google{Client: srv.Client(), Poll: time.Millisecond}

	ep := &gateway.Endpoint{ID: "google/imagen-4", ModelName: "imagen-4.0-generate-001", ExtraBody: map[string]any{"personGeneration": "allow_adult"}}
	res, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindImage, Model: ep.ModelName, Prompt: "a fox", N: 2, Size: "1536x1024"}, func(float64, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "g-key" || gotPath != "/models/imagen-4.0-generate-001:predict" {
		t.Errorf("key=%q path=%q", gotKey, gotPath)
	}
	params, _ := gotBody["parameters"].(map[string]any)
	inst, _ := gotBody["instances"].([]any)
	if params["sampleCount"] != float64(2) || params["aspectRatio"] != "4:3" || params["personGeneration"] != "allow_adult" || len(inst) != 1 || inst[0].(map[string]any)["prompt"] != "a fox" {
		t.Errorf("imagen body = %v", gotBody)
	}
	// One prediction was filtered, one came back.
	if len(res.Outputs) != 1 || res.Outputs[0].MIME != "image/png" || res.Outputs[0].Width != 5 {
		t.Errorf("imagen outputs = %+v", res.Outputs)
	}

	src := Decode(pngBytes(3, 3))
	ep2 := &gateway.Endpoint{ID: "google/veo-3", ModelName: "veo-3.0-generate-001"}
	res, err = eng.Generate(context.Background(), p, ep2, &Request{Kind: KindVideo, Model: ep2.ModelName, Prompt: "pan", Seconds: 8, Size: "16:9", Source: &src}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/models/veo-3.0-generate-001:predictLongRunning" {
		t.Errorf("veo path = %q", gotPath)
	}
	params, _ = gotBody["parameters"].(map[string]any)
	inst, _ = gotBody["instances"].([]any)
	img, _ := inst[0].(map[string]any)["image"].(map[string]any)
	if params["aspectRatio"] != "16:9" || params["durationSeconds"] != float64(8) || img["mimeType"] != "image/png" || img["bytesBase64Encoded"] == "" {
		t.Errorf("veo body = %v", gotBody)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].MIME != "video/mp4" || res.Outputs[0].Seconds != 8 || res.ProviderJobID != "models/veo-3.0-generate-001/operations/op-1" {
		t.Errorf("veo outputs = %+v job=%q", res.Outputs, res.ProviderJobID)
	}
	if _, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindEdit, Model: ep.ModelName, Prompt: "x", Source: &src}, nil); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("edit should be refused: %v", err)
	}
}

func TestAspectRatio(t *testing.T) {
	for in, want := range map[string]string{"": "", "auto": "", "16:9": "16:9", "1024x1024": "1:1", "1536x1024": "4:3", "1024x1536": "3:4", "1280x720": "16:9", "720x1280": "9:16", "square_hd": ""} {
		if got := aspectRatio(in); got != want {
			t.Errorf("aspectRatio(%q) = %q want %q", in, got, want)
		}
	}
}
