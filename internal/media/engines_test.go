package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

func videoEndpoint(id, engine string, perSecond float64, seconds ...int) *gateway.Endpoint {
	return &gateway.Endpoint{ID: id, ProviderID: strings.Split(id, "/")[0], ModelName: "v", Enabled: true,
		Capabilities: gateway.Capabilities{Media: &gateway.MediaCaps{Engine: engine, Video: true, ImageToVideo: true, Sizes: []string{"1280x720"}, MaxSeconds: 12, Seconds: seconds}},
		Pricing:      gateway.Pricing{PerSecond: perSecond}}
}

// putImage stores a source image as an attachment owned by uid.
func putImage(t *testing.T, svc *Service, db *fakeStore, uid uuid.UUID, mime string) store.Attachment {
	t.Helper()
	data := pngBytes(6, 6)
	key, err := blob.PutBytes(context.Background(), svc.Blobs, data)
	if err != nil {
		t.Fatal(err)
	}
	att, _ := db.CreateAttachment(context.Background(), store.CreateAttachmentParams{UserID: uid, BlobKey: key, Mime: mime, Bytes: int64(len(data))})
	return att
}

func TestEditJobReadsItsSource(t *testing.T) {
	eng := &fakeEngine{}
	ep := imageEndpoint("openai/img", "fake", 0.04)
	ep.Capabilities.Media.ImageEdit = true
	svc, db := newService(t, &memRecorder{}, map[string]Engine{"fake": eng}, ep)
	ctx := context.Background()
	uid, pid := uuid.New(), uuid.New()
	att := putImage(t, svc, db, uid, "image/png")

	// A source turns an image request into an edit.
	job, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "make it blue", SourceAttachmentID: att.ID.String()}})
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != KindEdit {
		t.Errorf("kind = %s", job.Kind)
	}
	if err := svc.Run(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetMediaJob(ctx, job.ID)
	if got.Status != StatusDone || eng.last == nil || eng.last.Kind != KindEdit || eng.last.Source == nil || eng.last.Source.Width != 6 || eng.last.Source.MIME != "image/png" {
		t.Errorf("job = %+v last = %+v", got, eng.last)
	}

	// Somebody else's attachment, unless a media job in the project made it.
	other := putImage(t, svc, db, uuid.New(), "image/png")
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindEdit, Inputs: Inputs{Prompt: "x", SourceAttachmentID: other.ID.String()}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("foreign source: %v", err)
	}
	outID := got.OutputAttachmentIds[0] // produced by this project's job, owned by uid; give it to another member
	db.mu.Lock()
	a := db.atts[outID]
	a.UserID = uuid.New()
	db.atts[outID] = a
	db.mu.Unlock()
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindEdit, Inputs: Inputs{Prompt: "x", SourceAttachmentID: outID.String()}}); err != nil {
		t.Errorf("project output as source: %v", err)
	}
	// Not an image, or no such attachment, or an edit without a source.
	pdf := putImage(t, svc, db, uid, "application/pdf")
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindEdit, Inputs: Inputs{Prompt: "x", SourceAttachmentID: pdf.ID.String()}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("pdf source: %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindEdit, Inputs: Inputs{Prompt: "x", SourceAttachmentID: uuid.NewString()}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("missing source: %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindEdit, Inputs: Inputs{Prompt: "x"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("edit without source: %v", err)
	}
	// An endpoint that only generates is skipped for edits.
	ep.Capabilities.Media.ImageEdit = false
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindEdit, Inputs: Inputs{Prompt: "x", SourceAttachmentID: att.ID.String()}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "edit") {
		t.Errorf("no edit endpoint: %v", err)
	}
}

func TestEditRoutesPastEndpointsThatCannotEdit(t *testing.T) {
	// openai/img only generates; fal/img only edits. The policy prefers
	// openai/img, but an edit must land on fal/img, and a plain generation
	// must never land on the edit-only endpoint.
	gen := imageEndpoint("openai/img", "fake", 0.04)
	edit := imageEndpoint("fal/img", "fake", 0.02)
	edit.Capabilities.Media.Image, edit.Capabilities.Media.ImageEdit = false, true
	eng := &fakeEngine{}
	svc, db := newService(t, &memRecorder{}, map[string]Engine{"fake": eng}, gen, edit)
	ctx := context.Background()
	uid, pid := uuid.New(), uuid.New()
	att := putImage(t, svc, db, uid, "image/png")
	job, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "bluer", SourceAttachmentID: att.ID.String()}})
	if err != nil {
		t.Fatal(err)
	}
	var in Inputs
	_ = json.Unmarshal(job.Inputs, &in)
	if in.EstimateUSD != 0.02 {
		t.Errorf("estimate should be the edit endpoint's: %+v", in)
	}
	_ = svc.Run(ctx, job.ID)
	got, _ := db.GetMediaJob(ctx, job.ID)
	if got.Status != StatusDone || *got.EndpointID != "fal/img" || eng.calls != 1 {
		t.Errorf("edit job = %+v calls=%d", got, eng.calls)
	}
	job2, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "a cat"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Run(ctx, job2.ID)
	got, _ = db.GetMediaJob(ctx, job2.ID)
	if got.Status != StatusDone || *got.EndpointID != "openai/img" {
		t.Errorf("generation job = %+v", got)
	}
	// Image to video needs an endpoint that takes a source.
	t2v := videoEndpoint("openai/sora", "fake", 0.1)
	t2v.Capabilities.Media.ImageToVideo = false
	svc2, db2 := newService(t, &memRecorder{}, map[string]Engine{"fake": &fakeEngine{}}, t2v)
	att2 := putImage(t, svc2, db2, uid, "image/png")
	if _, err := svc2.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindVideo, Inputs: Inputs{Prompt: "pan", SourceAttachmentID: att2.ID.String()}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("image to video with a text-only video endpoint: %v", err)
	}
	if _, err := svc2.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindVideo, Inputs: Inputs{Prompt: "waves"}}); err != nil {
		t.Errorf("text to video: %v", err)
	}
}

func TestVideoJobLengthsAndPricing(t *testing.T) {
	eng := &fakeEngine{}
	rec := &memRecorder{}
	svc, db := newService(t, rec, map[string]Engine{"fake": eng}, videoEndpoint("openai/sora", "fake", 0.10, 4, 8, 12))
	ctx := context.Background()
	uid, pid := uuid.New(), uuid.New()
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindVideo, Inputs: Inputs{Prompt: "waves", Seconds: 5}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "4, 8, 12") {
		t.Errorf("odd length: %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindVideo, Inputs: Inputs{Prompt: "waves", N: 2}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("two videos: %v", err)
	}
	job, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindVideo, Inputs: Inputs{Prompt: "waves"}})
	if err != nil {
		t.Fatal(err)
	}
	var in Inputs
	_ = json.Unmarshal(job.Inputs, &in)
	if in.Seconds != 4 || in.EstimateUSD < 0.39 || in.EstimateUSD > 0.41 {
		t.Errorf("defaults: %+v", in)
	}
	if err := svc.Run(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetMediaJob(ctx, job.ID)
	atts, _ := svc.Outputs(ctx, &got)
	if got.Status != StatusDone || eng.last.Seconds != 4 || len(atts) != 1 || atts[0].Mime != "video/mp4" || !strings.HasSuffix(atts[0].Filename, ".mp4") || got.CostUsd < 0.39 || got.CostUsd > 0.41 {
		t.Errorf("job = %+v atts = %+v", got, atts)
	}
	if recs := rec.wait(t, 1); recs[0].Metadata.TaskClass != gateway.TaskVideo {
		t.Errorf("ledger = %+v", recs[0])
	}
	// Image to video needs the capability.
	att := putImage(t, svc, db, uid, "image/png")
	job2, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindVideo, Inputs: Inputs{Prompt: "pan", Seconds: 8, SourceAttachmentID: att.ID.String()}})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Run(ctx, job2.ID)
	if eng.last.Source == nil || eng.last.Seconds != 8 {
		t.Errorf("image to video request = %+v", eng.last)
	}
}

func TestVideoToolReturnsVideos(t *testing.T) {
	svc, db := newService(t, &memRecorder{}, map[string]Engine{"fake": &fakeEngine{}}, videoEndpoint("openai/sora", "fake", 0.1))
	svc.Enqueue = func(ctx context.Context, id uuid.UUID) error {
		go func() { _ = svc.Run(context.Background(), id) }()
		return nil
	}
	conv := store.Conversation{ID: uuid.New(), ProjectID: uuid.New()}
	db.conv[conv.ID] = conv
	tool := NewVideoTool(svc, db)
	tool.Wait, tool.Poll = 10*time.Second, 10*time.Millisecond
	if tool.Def().Name != ToolNameVideo {
		t.Errorf("name = %s", tool.Def().Name)
	}
	res, err := tool.Call(context.Background(), agent.ToolCtx{ConversationID: conv.ID, UserID: uuid.New()}, json.RawMessage(`{"prompt":"waves","seconds":5}`))
	if err != nil || res.IsError {
		t.Fatalf("tool: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.Data)
	var data struct {
		Kind   string `json:"kind"`
		Videos []struct {
			URL  string `json:"url"`
			MIME string `json:"mime"`
		} `json:"videos"`
	}
	_ = json.Unmarshal(raw, &data)
	if data.Kind != KindVideo || len(data.Videos) != 1 || data.Videos[0].MIME != "video/mp4" || !strings.Contains(res.Text, "video(s)") {
		t.Errorf("result = %+v text=%q", data, res.Text)
	}
}

func TestOpenAIImagesEdit(t *testing.T) {
	var gotFields map[string]string
	var gotFile []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			http.Error(w, "wrong route "+r.URL.Path, 404)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		gotFields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			gotFields[k] = v[0]
		}
		f, _, err := r.FormFile("image")
		if err != nil {
			http.Error(w, "no image: "+err.Error(), 400)
			return
		}
		gotFile, _ = io.ReadAll(f)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(pngBytes(2, 2))}}})
	}))
	defer srv.Close()
	p := &gateway.Provider{ID: "openai", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL + "/v1", APIKey: "sk"}
	ep := &gateway.Endpoint{ID: "openai/gpt-image-1", ModelName: "gpt-image-1"}
	src := Decode(pngBytes(6, 6))
	res, err := (&OpenAIImages{Client: srv.Client()}).Generate(context.Background(), p, ep, &Request{Kind: KindEdit, Model: "gpt-image-1", Prompt: "bluer", N: 1, Size: "1024x1024", Source: &src}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotFields["model"] != "gpt-image-1" || gotFields["prompt"] != "bluer" || gotFields["size"] != "1024x1024" || len(gotFile) != len(src.Data) {
		t.Errorf("fields = %v file = %d bytes", gotFields, len(gotFile))
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Width != 2 {
		t.Errorf("outputs = %+v", res.Outputs)
	}
	if _, err := (&OpenAIImages{}).Generate(context.Background(), p, ep, &Request{Kind: KindEdit, Model: "x", Prompt: "x"}, nil); err == nil {
		t.Error("edit without source should fail")
	}
}

func TestOpenAIVideosEngine(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	var created map[string]any
	var gotRef []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos":
			if r.Header.Get("Authorization") != "Bearer sk" {
				http.Error(w, "no auth", 401)
				return
			}
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				_ = r.ParseMultipartForm(1 << 20)
				created = map[string]any{}
				for k, v := range r.MultipartForm.Value {
					created[k] = v[0]
				}
				f, _, err := r.FormFile("input_reference")
				if err == nil {
					gotRef, _ = io.ReadAll(f)
				}
			} else {
				_ = json.NewDecoder(r.Body).Decode(&created)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "video_1", "status": "queued", "progress": 0, "seconds": "4"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video_1":
			polls++
			st, pr := "in_progress", 50
			if polls >= 2 {
				st, pr = "completed", 100
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "video_1", "status": st, "progress": pr, "seconds": "4"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video_1/content":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("mp4 bytes"))
		default:
			http.Error(w, "wrong route "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	p := &gateway.Provider{ID: "openai", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL + "/v1", APIKey: "sk"}
	ep := &gateway.Endpoint{ID: "openai/sora-2", ModelName: "sora-2"}
	eng := &OpenAIVideos{Client: srv.Client(), Poll: time.Millisecond}
	var progress []float64
	var pid string
	res, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindVideo, Model: "sora-2", Prompt: "waves", Seconds: 4, Size: "1280x720"}, func(f float64, id string) { progress = append(progress, f); pid = id })
	if err != nil {
		t.Fatal(err)
	}
	if created["model"] != "sora-2" || created["seconds"] != "4" || created["size"] != "1280x720" {
		t.Errorf("create body = %v", created)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].MIME != "video/mp4" || res.Outputs[0].Seconds != 4 || res.Outputs[0].Width != 1280 || string(res.Outputs[0].Data) != "mp4 bytes" || res.ProviderJobID != "video_1" || pid != "video_1" {
		t.Errorf("result = %+v", res)
	}
	if len(progress) < 3 || progress[len(progress)-1] != 1 {
		t.Errorf("progress = %v", progress)
	}
	// Image to video uploads the source as input_reference.
	mu.Lock()
	polls = 0
	mu.Unlock()
	src := Decode(pngBytes(4, 4))
	if _, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindVideo, Model: "sora-2", Prompt: "pan", Seconds: 4, Source: &src}, nil); err != nil {
		t.Fatal(err)
	}
	if len(gotRef) != len(src.Data) || created["prompt"] != "pan" {
		t.Errorf("reference = %d bytes body = %v", len(gotRef), created)
	}
	if _, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindImage, Model: "x", Prompt: "x"}, nil); err == nil {
		t.Error("image should be refused")
	}
}

func TestFalEngine(t *testing.T) {
	var mu sync.Mutex
	var gotIn map[string]any
	var gotAuth, gotPath string
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		base := "http://" + r.Host
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/fal-ai/"):
			gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotIn)
			_ = json.NewEncoder(w).Encode(map[string]any{"request_id": "req-1", "status_url": base + r.URL.Path + "/requests/req-1/status", "response_url": base + r.URL.Path + "/requests/req-1"})
		case strings.HasSuffix(r.URL.Path, "/requests/req-1/status"):
			polls++
			st := "IN_PROGRESS"
			if polls >= 2 {
				st = "COMPLETED"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": st, "queue_position": 0})
		case strings.HasSuffix(r.URL.Path, "/requests/req-1"):
			if strings.Contains(r.URL.Path, "video") {
				_ = json.NewEncoder(w).Encode(map[string]any{"video": map[string]any{"url": base + "/files/out.mp4", "content_type": "video/mp4"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{{"url": base + "/files/out.png", "content_type": "image/png", "width": 7, "height": 7}}, "seed": 1})
		case r.URL.Path == "/files/out.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes(7, 7))
		case r.URL.Path == "/files/out.mp4":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("mp4"))
		default:
			http.Error(w, "wrong route "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	p := &gateway.Provider{ID: "fal", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL, APIKey: "fal-key"}
	ep := &gateway.Endpoint{ID: "fal/flux-dev", ModelName: "fal-ai/flux/dev", ExtraBody: map[string]any{"guidance_scale": 3.5}}
	eng := &Fal{Client: srv.Client(), Poll: time.Millisecond}
	res, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindImage, Model: "fal-ai/flux/dev", Prompt: "a fox", N: 2, Size: "1344x768"}, func(float64, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Key fal-key" || gotPath != "/fal-ai/flux/dev" || gotIn["prompt"] != "a fox" || gotIn["num_images"] != float64(2) || gotIn["guidance_scale"] != 3.5 {
		t.Errorf("request = %v auth=%q path=%q", gotIn, gotAuth, gotPath)
	}
	if sz, _ := gotIn["image_size"].(map[string]any); sz["width"] != float64(1344) || sz["height"] != float64(768) {
		t.Errorf("image_size = %v", gotIn["image_size"])
	}
	if len(res.Outputs) != 1 || res.Outputs[0].MIME != "image/png" || res.Outputs[0].Width != 7 || res.ProviderJobID != "req-1" {
		t.Errorf("outputs = %+v", res)
	}
	// Keyword sizes pass through; a source becomes image_url; video gets duration.
	mu.Lock()
	polls = 0
	mu.Unlock()
	src := Decode(pngBytes(3, 3))
	ep2 := &gateway.Endpoint{ID: "fal/wan", ModelName: "fal-ai/wan/v2.2/image-to-video"}
	res, err = eng.Generate(context.Background(), p, ep2, &Request{Kind: KindVideo, Model: ep2.ModelName, Prompt: "pan", Seconds: 5, Size: "16:9", Source: &src}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotIn["duration"] != "5" || gotIn["aspect_ratio"] != "16:9" || !strings.HasPrefix(gotIn["image_url"].(string), "data:image/png;base64,") {
		t.Errorf("video request = %v", gotIn)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].MIME != "video/mp4" || res.Outputs[0].Seconds != 5 {
		t.Errorf("video outputs = %+v", res.Outputs)
	}
}

func TestComfyUIEngine(t *testing.T) {
	dir := t.TempDir()
	tmpl := `{"3":{"class_type":"KSampler","inputs":{"seed":{{seed}},"steps":20}},"4":{"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"{{model}}"}},"5":{"class_type":"EmptyLatentImage","inputs":{"width":{{width}},"height":{{height}},"batch_size":{{batch}}}},"6":{"class_type":"CLIPTextEncode","inputs":{"text":"{{prompt}}"}},"7":{"class_type":"CLIPTextEncode","inputs":{"text":"{{negative}}"}},"10":{"class_type":"LoadImage","inputs":{"image":"{{source}}"}}}`
	if err := os.WriteFile(filepath.Join(dir, "sdxl.json"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var gotPrompt map[string]any
	uploaded := ""
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/upload/image":
			_ = r.ParseMultipartForm(1 << 20)
			_, hdr, err := r.FormFile("image")
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			uploaded = hdr.Filename
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "src.png", "subfolder": "", "type": "input"})
		case r.Method == http.MethodPost && r.URL.Path == "/prompt":
			var body struct {
				Prompt map[string]any `json:"prompt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotPrompt = body.Prompt
			_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": "p-1", "number": 3, "node_errors": map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/history/p-1":
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"p-1": map[string]any{
				"outputs": map[string]any{"9": map[string]any{"images": []map[string]any{{"filename": "ComfyUI_00001_.png", "subfolder": "", "type": "output"}}}},
				"status":  map[string]any{"status_str": "success", "completed": true, "messages": []any{}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/view":
			if r.URL.Query().Get("filename") != "ComfyUI_00001_.png" || r.URL.Query().Get("type") != "output" {
				http.Error(w, "bad view query "+r.URL.RawQuery, 400)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes(9, 9))
		default:
			http.Error(w, "wrong route "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	p := &gateway.Provider{ID: "comfyui", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL}
	ep := &gateway.Endpoint{ID: "comfyui/sdxl", ModelName: "sd_xl_base_1.0.safetensors", ExtraBody: map[string]any{"workflow": "sdxl.json", "negative": "blurry"}}
	eng := &ComfyUI{Client: srv.Client(), Workflows: dir, Poll: time.Millisecond, Seed: func() int64 { return 42 }}
	src := Decode(pngBytes(2, 2))
	res, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindEdit, Model: ep.ModelName, Prompt: `a "quoted" fox`, N: 2, Size: "1344x768", Source: &src}, func(float64, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if uploaded == "" {
		t.Error("source was not uploaded")
	}
	node := func(id, key string) any { return gotPrompt[id].(map[string]any)["inputs"].(map[string]any)[key] }
	if node("3", "seed") != float64(42) || node("4", "ckpt_name") != ep.ModelName || node("5", "width") != float64(1344) || node("5", "height") != float64(768) || node("5", "batch_size") != float64(2) ||
		node("6", "text") != `a "quoted" fox` || node("7", "text") != "blurry" || node("10", "image") != "src.png" {
		t.Errorf("filled workflow = %v", gotPrompt)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Width != 9 || res.ProviderJobID != "p-1" {
		t.Errorf("outputs = %+v", res)
	}
	// A workflow outside the directory, or none at all, is refused.
	bad := &gateway.Endpoint{ID: "comfyui/bad", ExtraBody: map[string]any{"workflow": "../secrets.json"}}
	if _, err := eng.Generate(context.Background(), p, bad, &Request{Kind: KindImage, Prompt: "x"}, nil); err == nil || !strings.Contains(err.Error(), "workflows directory") {
		t.Errorf("escaping workflow: %v", err)
	}
	if _, err := eng.Generate(context.Background(), p, &gateway.Endpoint{ID: "comfyui/none"}, &Request{Kind: KindImage, Prompt: "x"}, nil); err == nil || !strings.Contains(err.Error(), "extra_body.workflow") {
		t.Errorf("no workflow: %v", err)
	}
	// Inline workflows work too, and a failed prompt reports the node's error.
	inline := map[string]any{"1": map[string]any{"class_type": "X", "inputs": map[string]any{"text": "{{prompt}}"}}}
	epi := &gateway.Endpoint{ID: "comfyui/inline", ExtraBody: map[string]any{"workflow": inline}}
	mu.Lock()
	polls = 10
	mu.Unlock()
	if _, err := eng.Generate(context.Background(), p, epi, &Request{Kind: KindImage, Prompt: "y"}, nil); err != nil {
		t.Errorf("inline workflow: %v", err)
	}
	if got := comfyError([]json.RawMessage{json.RawMessage(`["execution_start",{}]`), json.RawMessage(`["execution_error",{"node_type":"KSampler","exception_message":"out of memory"}]`)}); got != "KSampler: out of memory" {
		t.Errorf("comfyError = %q", got)
	}
}

func TestDimensionsAndDefaults(t *testing.T) {
	if w, h, ok := Dimensions("1536x1024"); !ok || w != 1536 || h != 1024 {
		t.Errorf("1536x1024 -> %d %d %v", w, h, ok)
	}
	for _, s := range []string{"auto", "", "x", "16:9", "ax1"} {
		if _, _, ok := Dimensions(s); ok {
			t.Errorf("%q should not parse", s)
		}
	}
	ep := &gateway.Endpoint{Capabilities: gateway.Capabilities{Media: &gateway.MediaCaps{MaxSeconds: 3}}}
	if defaultSeconds(ep, 0) != 3 || defaultSeconds(ep, 2) != 2 {
		t.Error("max_seconds should cap the default")
	}
	ep.Capabilities.Media.Seconds = []int{8, 4}
	if defaultSeconds(ep, 0) != 8 {
		t.Error("the first listed length is the default")
	}
	if defaultSeconds(&gateway.Endpoint{Capabilities: gateway.Capabilities{Media: &gateway.MediaCaps{}}}, 0) != 5 {
		t.Error("default is 5")
	}
}
