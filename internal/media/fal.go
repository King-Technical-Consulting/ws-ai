package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jking323/ws/internal/gateway"
)

// EngineFal is the Capabilities.Media.Engine value for fal.ai's queue API,
// which fronts hundreds of image and video models (FLUX, Kontext, Wan,
// Kling, Veo, …) behind one request shape.
const EngineFal = "fal"

// Fal runs jobs through fal's queue: POST {base}/{model} with the model's
// input returns a request id and the status and response URLs; the status
// URL is polled until COMPLETED; the response URL carries the model's
// output, which names the files by URL. The provider's base URL is
// https://queue.fal.run and its key goes in `Authorization: Key …`.
//
// ws fills the fields most fal models share: prompt, num_images and
// image_size for images, image_url (a data URL of the source) for edits
// and image to video, duration for video. Anything model-specific goes in
// the endpoint's extra_body, which wins over these.
type Fal struct {
	Client *http.Client
	// Poll is the status poll interval (default 3 s).
	Poll time.Duration
}

func (f *Fal) poll() time.Duration {
	if f.Poll > 0 {
		return f.Poll
	}
	return 3 * time.Second
}

// Generate implements Engine.
func (f *Fal) Generate(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *Request, progress Progress) (*Result, error) {
	c := defaultClient(f.Client)
	base := strings.TrimRight(p.BaseURL, "/")
	model := strings.Trim(req.Model, "/")
	if model == "" {
		return nil, errors.New("fal: the endpoint names no model")
	}
	in := map[string]any{"prompt": req.Prompt}
	switch req.Kind {
	case KindImage, KindEdit:
		n := req.N
		if n <= 0 {
			n = 1
		}
		in["num_images"] = n
		if req.Size != "" {
			if w, h, ok := Dimensions(req.Size); ok {
				in["image_size"] = map[string]int{"width": w, "height": h}
			} else {
				in["image_size"] = req.Size
			}
		}
	case KindVideo:
		if req.Seconds > 0 {
			in["duration"] = strconv.Itoa(req.Seconds)
		}
		if req.Size != "" {
			if _, _, ok := Dimensions(req.Size); !ok {
				in["aspect_ratio"] = req.Size
			}
		}
	case KindUpscale:
		if req.Prompt == "" {
			delete(in, "prompt")
		}
		scale := req.Scale
		if scale <= 0 {
			scale = 2
		}
		in["upscale_factor"] = scale
	}
	if req.Source != nil {
		in["image_url"] = dataURL(req.Source)
	}
	if req.Mask != nil {
		in["mask_url"] = dataURL(req.Mask)
	}
	for k, v := range ep.ExtraBody {
		in[k] = v
	}
	raw, _ := json.Marshal(in)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+model, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	authorize(hreq, p, "Key")
	var q struct {
		RequestID   string `json:"request_id"`
		StatusURL   string `json:"status_url"`
		ResponseURL string `json:"response_url"`
	}
	if err := doJSON(ctx, c, "fal", hreq, &q); err != nil {
		return nil, err
	}
	if q.RequestID == "" {
		return nil, errors.New("fal: the queue reply carried no request id")
	}
	if q.StatusURL == "" {
		q.StatusURL = base + "/" + model + "/requests/" + q.RequestID + "/status"
	}
	if q.ResponseURL == "" {
		q.ResponseURL = base + "/" + model + "/requests/" + q.RequestID
	}
	if progress != nil {
		progress(0.05, q.RequestID)
	}
	for {
		if err := sleepCtx(ctx, f.poll()); err != nil {
			return nil, err
		}
		sreq, err := http.NewRequestWithContext(ctx, http.MethodGet, q.StatusURL, nil)
		if err != nil {
			return nil, err
		}
		authorize(sreq, p, "Key")
		var st struct {
			Status        string `json:"status"`
			QueuePosition int    `json:"queue_position"`
		}
		if err := doJSON(ctx, c, "fal", sreq, &st); err != nil {
			return nil, err
		}
		if st.Status == "COMPLETED" {
			break
		}
		if progress != nil {
			if st.Status == "IN_PROGRESS" {
				progress(0.5, q.RequestID)
			} else {
				progress(0.1, q.RequestID)
			}
		}
	}
	rreq, err := http.NewRequestWithContext(ctx, http.MethodGet, q.ResponseURL, nil)
	if err != nil {
		return nil, err
	}
	authorize(rreq, p, "Key")
	var out map[string]json.RawMessage
	if err := doJSON(ctx, c, "fal", rreq, &out); err != nil {
		return nil, err
	}
	files := falFiles(out)
	if len(files) == 0 {
		// A failed request answers the response URL with an error body
		// (handled above) or an output without files.
		if d, ok := out["detail"]; ok {
			return nil, fmt.Errorf("fal: %s", strings.Trim(string(d), `"`))
		}
		return nil, fmt.Errorf("fal: %w", errNoOutput)
	}
	res := &Result{ProviderJobID: q.RequestID}
	for _, fl := range files {
		data, ctype, err := fetchBytes(ctx, c, "fal", fl.URL, nil, "", MaxVideoBytes)
		if err != nil {
			return nil, err
		}
		mime := fl.ContentType
		if mime == "" {
			mime = ctype
		}
		o := Output{Data: data, MIME: outputMIME(mime, data), Width: fl.Width, Height: fl.Height}
		if strings.HasPrefix(o.MIME, "image/") {
			if o.Width == 0 || o.Height == 0 {
				d := Decode(data)
				o.Width, o.Height = d.Width, d.Height
			}
		} else if strings.HasPrefix(o.MIME, "video/") {
			o.Seconds = float64(req.Seconds)
		}
		res.Outputs = append(res.Outputs, o)
	}
	if progress != nil {
		progress(1, q.RequestID)
	}
	return res, nil
}

// falFile is one output file as fal describes it.
type falFile struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// falFiles finds the files in a model's output: images, image, video or
// videos, as an object or a list.
func falFiles(out map[string]json.RawMessage) []falFile {
	var files []falFile
	for _, key := range []string{"images", "image", "video", "videos", "output"} {
		raw, ok := out[key]
		if !ok {
			continue
		}
		var one falFile
		if json.Unmarshal(raw, &one) == nil && one.URL != "" {
			files = append(files, one)
			continue
		}
		var many []falFile
		if json.Unmarshal(raw, &many) == nil {
			for _, f := range many {
				if f.URL != "" {
					files = append(files, f)
				}
			}
		}
	}
	return files
}

func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
