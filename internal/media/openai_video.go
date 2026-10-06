package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jking323/ws/internal/gateway"
)

// EngineOpenAIVideos is the Capabilities.Media.Engine value for the OpenAI
// Videos API (Sora 2 and Sora 2 Pro).
const EngineOpenAIVideos = "openai_videos"

// OpenAIVideos runs video jobs through the OpenAI Videos API on the
// provider's base URL (…/v1): POST /videos starts a render (with a source
// image as input_reference for image to video), GET /videos/{id} is
// polled until it completes, and GET /videos/{id}/content is the MP4.
//
// Field names (model, prompt, seconds as a string, size, input_reference,
// status, progress) follow the API as documented at the time of writing
// and were not exercised against the live service from here.
type OpenAIVideos struct {
	Client *http.Client
	// Poll is the status poll interval (default 5 s).
	Poll time.Duration
}

// MaxVideoBytes bounds one video read from a provider.
const MaxVideoBytes = 512 << 20

func (o *OpenAIVideos) poll() time.Duration {
	if o.Poll > 0 {
		return o.Poll
	}
	return 5 * time.Second
}

// Generate implements Engine.
func (o *OpenAIVideos) Generate(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *Request, progress Progress) (*Result, error) {
	if req.Kind != KindVideo {
		return nil, fmt.Errorf("openai videos: %s jobs are not supported", req.Kind)
	}
	c := defaultClient(o.Client)
	base := strings.TrimRight(p.BaseURL, "/")
	fields := map[string]string{"model": req.Model, "prompt": req.Prompt}
	if req.Seconds > 0 {
		fields["seconds"] = strconv.Itoa(req.Seconds)
	}
	if req.Size != "" {
		fields["size"] = req.Size
	}
	for k, v := range ep.ExtraBody {
		fields[k] = fmt.Sprint(v)
	}
	var hreq *http.Request
	var err error
	if req.Source != nil {
		body, ctype, ferr := form(fields, map[string]*Output{"input_reference": req.Source})
		if ferr != nil {
			return nil, ferr
		}
		hreq, err = http.NewRequestWithContext(ctx, http.MethodPost, base+"/videos", body)
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Content-Type", ctype)
	} else {
		body := map[string]any{}
		for k, v := range fields {
			body[k] = v
		}
		for k, v := range ep.ExtraBody {
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		hreq, err = http.NewRequestWithContext(ctx, http.MethodPost, base+"/videos", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Content-Type", "application/json")
	}
	authorize(hreq, p, "Bearer")
	type video struct {
		ID       string  `json:"id"`
		Status   string  `json:"status"`
		Progress float64 `json:"progress"`
		Seconds  string  `json:"seconds"`
		Error    *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	var v video
	if err := doJSON(ctx, c, "openai videos", hreq, &v); err != nil {
		return nil, err
	}
	if v.ID == "" {
		return nil, errors.New("openai videos: the response carried no video id")
	}
	if progress != nil {
		progress(0.02, v.ID)
	}
	for v.Status != "completed" {
		if v.Status == "failed" || v.Status == "cancelled" || v.Status == "error" {
			msg := v.Status
			if v.Error != nil && v.Error.Message != "" {
				msg += ": " + v.Error.Message
			}
			return nil, fmt.Errorf("openai videos: render %s", msg)
		}
		if err := sleepCtx(ctx, o.poll()); err != nil {
			return nil, err
		}
		greq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/videos/"+v.ID, nil)
		if err != nil {
			return nil, err
		}
		authorize(greq, p, "Bearer")
		if err := doJSON(ctx, c, "openai videos", greq, &v); err != nil {
			return nil, err
		}
		if progress != nil {
			f := v.Progress / 100
			if f > 0.98 {
				f = 0.98
			}
			if f < 0.02 {
				f = 0.02
			}
			progress(f, v.ID)
		}
	}
	data, ctype, err := fetchBytes(ctx, c, "openai videos", base+"/videos/"+v.ID+"/content", p, "Bearer", MaxVideoBytes)
	if err != nil {
		return nil, err
	}
	secs, _ := strconv.ParseFloat(v.Seconds, 64)
	if secs == 0 {
		secs = float64(req.Seconds)
	}
	out := Output{Data: data, MIME: outputMIME(ctype, data), Seconds: secs}
	if w, h, ok := Dimensions(req.Size); ok {
		out.Width, out.Height = w, h
	}
	if progress != nil {
		progress(1, v.ID)
	}
	return &Result{Outputs: []Output{out}, ProviderJobID: v.ID}, nil
}
