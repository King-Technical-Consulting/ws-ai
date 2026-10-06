package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jking323/ws/internal/gateway"
)

// EngineGoogle is the Capabilities.Media.Engine value for Google's Gemini
// API media models: Imagen for images and Veo for video. The provider's
// base URL is https://generativelanguage.googleapis.com/v1beta and its
// key (GEMINI_API_KEY) goes in the x-goog-api-key header. The endpoint's
// model is the model id (imagen-4.0-generate-001, veo-3.0-generate-001).
const EngineGoogle = "google"

// Google runs Imagen through POST models/{model}:predict (synchronous,
// base64 images in the reply) and Veo through models/{model}:
// predictLongRunning, polling the operation until done and downloading
// the video it names. Sizes are aspect ratios (1:1, 3:4, 4:3, 9:16,
// 16:9); a WxH size is mapped to the nearest one. Edits and masks are not
// offered here (Imagen's editing API is a different product).
type Google struct {
	Client *http.Client
	// Poll is the operation poll interval (default 5 s).
	Poll time.Duration
}

func (g *Google) poll() time.Duration {
	if g.Poll > 0 {
		return g.Poll
	}
	return 5 * time.Second
}

// Generate implements Engine.
func (g *Google) Generate(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *Request, progress Progress) (*Result, error) {
	c := defaultClient(g.Client)
	base := strings.TrimRight(p.BaseURL, "/")
	model := strings.TrimPrefix(strings.Trim(req.Model, "/"), "models/")
	if model == "" {
		return nil, errors.New("google: the endpoint names no model")
	}
	switch req.Kind {
	case KindImage:
		return g.imagen(ctx, c, p, ep, base, model, req, progress)
	case KindVideo:
		return g.veo(ctx, c, p, ep, base, model, req, progress)
	}
	return nil, fmt.Errorf("google: %s jobs are not supported", req.Kind)
}

// googleAuth sets the Gemini API key header and the provider's headers.
func googleAuth(req *http.Request, p *gateway.Provider) {
	if p == nil {
		return
	}
	if p.APIKey != "" {
		req.Header.Set("x-goog-api-key", p.APIKey)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
}

func (g *Google) imagen(ctx context.Context, c *http.Client, p *gateway.Provider, ep *gateway.Endpoint, base, model string, req *Request, progress Progress) (*Result, error) {
	n := req.N
	if n <= 0 {
		n = 1
	}
	params := map[string]any{"sampleCount": n}
	if ar := aspectRatio(req.Size); ar != "" {
		params["aspectRatio"] = ar
	}
	for k, v := range ep.ExtraBody {
		params[k] = v
	}
	body := map[string]any{"instances": []map[string]any{{"prompt": req.Prompt}}, "parameters": params}
	raw, _ := json.Marshal(body)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/models/"+model+":predict", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	googleAuth(hreq, p)
	if progress != nil {
		progress(0, "")
	}
	var out struct {
		Predictions []struct {
			Bytes    string `json:"bytesBase64Encoded"`
			MIME     string `json:"mimeType"`
			Filtered string `json:"raiFilteredReason"`
		} `json:"predictions"`
	}
	if err := doJSON(ctx, c, "google", hreq, &out); err != nil {
		return nil, err
	}
	res := &Result{}
	var filtered string
	for _, pr := range out.Predictions {
		if pr.Bytes == "" {
			if pr.Filtered != "" {
				filtered = pr.Filtered
			}
			continue
		}
		data, err := base64.StdEncoding.DecodeString(pr.Bytes)
		if err != nil {
			return nil, fmt.Errorf("google: decode image: %w", err)
		}
		o := Decode(data)
		o.MIME = outputMIME(pr.MIME, data)
		res.Outputs = append(res.Outputs, o)
	}
	if len(res.Outputs) == 0 {
		if filtered != "" {
			return nil, fmt.Errorf("google: the request was filtered: %s", filtered)
		}
		return nil, fmt.Errorf("google: %w", errNoOutput)
	}
	if progress != nil {
		progress(1, "")
	}
	return res, nil
}

func (g *Google) veo(ctx context.Context, c *http.Client, p *gateway.Provider, ep *gateway.Endpoint, base, model string, req *Request, progress Progress) (*Result, error) {
	inst := map[string]any{"prompt": req.Prompt}
	if req.Source != nil {
		inst["image"] = map[string]any{"bytesBase64Encoded": base64Encode(req.Source.Data), "mimeType": req.Source.MIME}
	}
	params := map[string]any{"numberOfVideos": 1}
	if ar := aspectRatio(req.Size); ar != "" {
		params["aspectRatio"] = ar
	}
	if req.Seconds > 0 {
		params["durationSeconds"] = req.Seconds
	}
	for k, v := range ep.ExtraBody {
		params[k] = v
	}
	body := map[string]any{"instances": []map[string]any{inst}, "parameters": params}
	raw, _ := json.Marshal(body)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/models/"+model+":predictLongRunning", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	googleAuth(hreq, p)
	var op googleOp
	if err := doJSON(ctx, c, "google", hreq, &op); err != nil {
		return nil, err
	}
	if op.Name == "" {
		return nil, errors.New("google: the operation has no name")
	}
	if progress != nil {
		progress(0.05, op.Name)
	}
	for !op.Done {
		if err := sleepCtx(ctx, g.poll()); err != nil {
			return nil, err
		}
		preq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+strings.TrimLeft(op.Name, "/"), nil)
		if err != nil {
			return nil, err
		}
		googleAuth(preq, p)
		op = googleOp{}
		if err := doJSON(ctx, c, "google", preq, &op); err != nil {
			return nil, err
		}
		if progress != nil && !op.Done {
			progress(0.5, op.Name)
		}
	}
	if op.Error != nil && op.Error.Message != "" {
		return nil, fmt.Errorf("google: %s", op.Error.Message)
	}
	res := &Result{ProviderJobID: op.Name}
	for _, s := range op.Response.GenerateVideoResponse.GeneratedSamples {
		uri := s.Video.URI
		if uri == "" {
			continue
		}
		dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
		if err != nil {
			return nil, err
		}
		googleAuth(dreq, p)
		data, ctype, err := fetchRequest(c, "google", dreq, MaxVideoBytes)
		if err != nil {
			return nil, err
		}
		res.Outputs = append(res.Outputs, Output{Data: data, MIME: outputMIME(ctype, data), Seconds: float64(req.Seconds)})
	}
	if len(res.Outputs) == 0 {
		if r := op.Response.GenerateVideoResponse.RaiMediaFilteredReasons; len(r) > 0 {
			return nil, fmt.Errorf("google: the request was filtered: %s", strings.Join(r, "; "))
		}
		return nil, fmt.Errorf("google: %w", errNoOutput)
	}
	if progress != nil {
		progress(1, op.Name)
	}
	return res, nil
}

// googleOp is a long-running operation as the Gemini API reports it.
type googleOp struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Response struct {
		GenerateVideoResponse struct {
			GeneratedSamples []struct {
				Video struct {
					URI string `json:"uri"`
				} `json:"video"`
			} `json:"generatedSamples"`
			RaiMediaFilteredReasons []string `json:"raiMediaFilteredReasons"`
		} `json:"generateVideoResponse"`
	} `json:"response"`
}

// aspectRatio maps a size to the ratio Google takes: a ratio keyword
// passes through, a WxH picks the nearest of 1:1, 3:4, 4:3, 9:16, 16:9,
// and anything else is left to the model.
func aspectRatio(size string) string {
	size = strings.TrimSpace(size)
	if size == "" || size == "auto" {
		return ""
	}
	ratios := map[string]float64{"1:1": 1, "3:4": 3.0 / 4, "4:3": 4.0 / 3, "9:16": 9.0 / 16, "16:9": 16.0 / 9}
	if _, ok := ratios[size]; ok {
		return size
	}
	w, h, ok := Dimensions(size)
	if !ok || h == 0 {
		return ""
	}
	want := float64(w) / float64(h)
	best, bestD := "", math.MaxFloat64
	for k, r := range ratios {
		if d := math.Abs(r - want); d < bestD || (d == bestD && k < best) {
			best, bestD = k, d
		}
	}
	return best
}
