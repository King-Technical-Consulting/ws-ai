package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // dimensions of jpeg outputs
	_ "image/png"  // dimensions of png outputs
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jking323/ws/internal/gateway"
)

// EngineOpenAIImages is the Capabilities.Media.Engine value for the OpenAI
// Images API (gpt-image-1 and the DALL-E models, and any server that
// implements POST /images/generations the same way).
const EngineOpenAIImages = "openai_images"

// OpenAIImages runs image jobs through the OpenAI Images API on the
// provider's base URL (…/v1): POST /images/generations with the prompt,
// size, count and quality; outputs come back base64 (gpt-image-1) or as
// short-lived URLs (DALL-E, asked for base64 explicitly), and are read
// into memory here. The call is synchronous; progress is 0 then 1.
type OpenAIImages struct {
	Client *http.Client
}

// MaxOutputBytes bounds one image read from the provider.
const MaxOutputBytes = 32 << 20

func (o *OpenAIImages) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// Generate implements Engine.
func (o *OpenAIImages) Generate(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *Request, progress Progress) (*Result, error) {
	if req.Kind != KindImage {
		return nil, fmt.Errorf("openai images: %s jobs are not supported", req.Kind)
	}
	n := req.N
	if n <= 0 {
		n = 1
	}
	body := map[string]any{"model": req.Model, "prompt": req.Prompt, "n": n}
	if req.Size != "" {
		body["size"] = req.Size
	}
	if req.Quality != "" {
		body["quality"] = req.Quality
	}
	if strings.HasPrefix(req.Model, "dall-e") {
		body["response_format"] = "b64_json"
	}
	for k, v := range ep.ExtraBody {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	url := strings.TrimRight(p.BaseURL, "/") + "/images/generations"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		hreq.Header.Set(k, v)
	}
	if progress != nil {
		progress(0, "")
	}
	resp, err := o.client().Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("openai images: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, int64(MaxOutputBytes)*int64(n)+1<<20))
	if err != nil {
		return nil, fmt.Errorf("openai images: read: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai images: HTTP %d: %s", resp.StatusCode, apiError(rb))
	}
	var out struct {
		Data []struct {
			B64           string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("openai images: decode: %w", err)
	}
	res := &Result{}
	for _, d := range out.Data {
		var data []byte
		switch {
		case d.B64 != "":
			data, err = base64.StdEncoding.DecodeString(d.B64)
			if err != nil {
				return nil, fmt.Errorf("openai images: bad base64: %w", err)
			}
		case d.URL != "":
			data, err = o.fetch(ctx, d.URL)
			if err != nil {
				return nil, err
			}
		default:
			continue
		}
		res.Outputs = append(res.Outputs, Decode(data))
		if d.RevisedPrompt != "" {
			res.RevisedPrompt = d.RevisedPrompt
		}
	}
	if out.Usage != nil {
		res.UsageKnown = true
		res.Usage = gateway.Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}
	}
	if len(res.Outputs) == 0 {
		return nil, errors.New("openai images: response carried no image")
	}
	if progress != nil {
		progress(1, "")
	}
	return res, nil
}

func (o *OpenAIImages) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai images: fetch output: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai images: fetch output: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxOutputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxOutputBytes {
		return nil, errors.New("openai images: output larger than 32 MB")
	}
	return b, nil
}

// Decode sniffs an image's type and dimensions. Unknown formats keep
// their sniffed MIME and no dimensions.
func Decode(data []byte) Output {
	o := Output{Data: data, MIME: http.DetectContentType(data)}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		o.Width, o.Height = cfg.Width, cfg.Height
	}
	return o
}

// apiError pulls the message out of an OpenAI-style error body.
func apiError(b []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
