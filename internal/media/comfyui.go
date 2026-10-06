package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jking323/ws/internal/gateway"
)

// EngineComfyUI is the Capabilities.Media.Engine value for a local ComfyUI
// server.
const EngineComfyUI = "comfyui"

// ComfyUI runs jobs on a ComfyUI server (the provider's base URL, usually
// http://host:8188) by filling a workflow template and queueing it.
//
// The template is an API-format workflow (ComfyUI: "Save (API format)"),
// named by the endpoint's extra_body.workflow as a file under Workflows
// or given inline as a JSON object. Before it is sent, ws substitutes the
// placeholders {{prompt}}, {{negative}}, {{width}}, {{height}}, {{seed}},
// {{batch}}, {{seconds}}, {{frames}}, {{source}} (the uploaded source
// image's name), {{mask}} (the uploaded mask's name, "" without one),
// {{scale}} (the upscale factor) and {{model}} (the endpoint's model name, for the
// checkpoint loader). Quote the string placeholders in the template
// ("{{prompt}}"); leave the numbers bare.
//
// Then: POST /prompt queues it, GET /history/{id} is polled until the
// prompt completes, and every image or video the history lists is read
// through GET /view. A source image is uploaded first with POST
// /upload/image.
type ComfyUI struct {
	Client *http.Client
	// Workflows is the directory workflow files are read from.
	Workflows string
	// Poll is the history poll interval (default 2 s).
	Poll time.Duration
	// Seed, when set, replaces the random seed (tests).
	Seed func() int64
}

func (c *ComfyUI) poll() time.Duration {
	if c.Poll > 0 {
		return c.Poll
	}
	return 2 * time.Second
}

// Generate implements Engine.
func (c *ComfyUI) Generate(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *Request, progress Progress) (*Result, error) {
	client := defaultClient(c.Client)
	base := strings.TrimRight(p.BaseURL, "/")
	tmpl, err := c.template(ep)
	if err != nil {
		return nil, err
	}
	source, mask := "", ""
	if req.Source != nil {
		name, err := c.upload(ctx, client, base, p, req.Source)
		if err != nil {
			return nil, err
		}
		source = name
	}
	if req.Mask != nil {
		name, err := c.upload(ctx, client, base, p, req.Mask)
		if err != nil {
			return nil, err
		}
		mask = name
	}
	wf, err := fill(tmpl, ep, req, source, mask, c.seed())
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"prompt": wf, "client_id": "ws"})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/prompt", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	authorize(hreq, p, "Bearer")
	var q struct {
		PromptID   string          `json:"prompt_id"`
		NodeErrors json.RawMessage `json:"node_errors"`
	}
	if err := doJSON(ctx, client, "comfyui", hreq, &q); err != nil {
		return nil, err
	}
	if q.PromptID == "" {
		return nil, errors.New("comfyui: the queue reply carried no prompt id")
	}
	if progress != nil {
		progress(0.1, q.PromptID)
	}
	type file struct {
		Filename  string `json:"filename"`
		Subfolder string `json:"subfolder"`
		Type      string `json:"type"`
		Format    string `json:"format"`
	}
	var files []file
	for {
		if err := sleepCtx(ctx, c.poll()); err != nil {
			return nil, err
		}
		greq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/history/"+q.PromptID, nil)
		if err != nil {
			return nil, err
		}
		authorize(greq, p, "Bearer")
		var hist map[string]struct {
			Outputs map[string]map[string]json.RawMessage `json:"outputs"`
			Status  struct {
				StatusStr string            `json:"status_str"`
				Completed bool              `json:"completed"`
				Messages  []json.RawMessage `json:"messages"`
			} `json:"status"`
		}
		if err := doJSON(ctx, client, "comfyui", greq, &hist); err != nil {
			return nil, err
		}
		h, ok := hist[q.PromptID]
		if !ok {
			if progress != nil {
				progress(0.5, q.PromptID)
			}
			continue
		}
		if h.Status.StatusStr == "error" {
			return nil, fmt.Errorf("comfyui: the workflow failed: %s", comfyError(h.Status.Messages))
		}
		if !h.Status.Completed && h.Status.StatusStr != "success" {
			continue
		}
		for _, node := range h.Outputs {
			for _, key := range []string{"images", "gifs", "videos"} {
				raw, ok := node[key]
				if !ok {
					continue
				}
				var fs []file
				if json.Unmarshal(raw, &fs) == nil {
					files = append(files, fs...)
				}
			}
		}
		break
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("comfyui: %w (the workflow has no save node, or it saved nothing)", errNoOutput)
	}
	res := &Result{ProviderJobID: q.PromptID}
	for _, fl := range files {
		if fl.Type == "temp" && len(files) > 1 {
			continue // previews next to a saved output
		}
		v := url.Values{"filename": {fl.Filename}, "subfolder": {fl.Subfolder}, "type": {fl.Type}}
		data, ctype, err := fetchBytes(ctx, client, "comfyui", base+"/view?"+v.Encode(), p, "Bearer", MaxVideoBytes)
		if err != nil {
			return nil, err
		}
		mime := ctype
		if strings.HasPrefix(fl.Format, "video/") {
			mime = strings.TrimPrefix(fl.Format, "video/h264-")
			if mime == "mp4" {
				mime = "video/mp4"
			} else {
				mime = fl.Format
			}
		}
		o := Output{Data: data, MIME: outputMIME(mime, data)}
		if strings.HasPrefix(o.MIME, "image/") {
			d := Decode(data)
			o.Width, o.Height = d.Width, d.Height
		} else {
			o.Seconds = float64(req.Seconds)
			if w, h, ok := Dimensions(req.Size); ok {
				o.Width, o.Height = w, h
			}
		}
		res.Outputs = append(res.Outputs, o)
	}
	if len(res.Outputs) == 0 {
		return nil, fmt.Errorf("comfyui: %w", errNoOutput)
	}
	if progress != nil {
		progress(1, q.PromptID)
	}
	return res, nil
}

// template loads the endpoint's workflow: extra_body.workflow is a file
// name under Workflows or an inline object.
func (c *ComfyUI) template(ep *gateway.Endpoint) (string, error) {
	raw, ok := ep.ExtraBody["workflow"]
	if !ok {
		return "", fmt.Errorf("comfyui: endpoint %s has no extra_body.workflow", ep.ID)
	}
	switch v := raw.(type) {
	case string:
		name := filepath.Clean(strings.TrimSpace(v))
		if name == "" || name == "." || filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return "", fmt.Errorf("comfyui: workflow %q must be a file name under the workflows directory", v)
		}
		dir := c.Workflows
		if dir == "" {
			dir = "infra/comfyui/workflows"
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("comfyui: workflow: %w", err)
		}
		return string(b), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("comfyui: workflow: %w", err)
		}
		return string(b), nil
	}
}

// fill substitutes the placeholders and parses the result.
func fill(tmpl string, ep *gateway.Endpoint, req *Request, source, mask string, seed int64) (map[string]any, error) {
	w, h := 1024, 1024
	if dw, dh, ok := Dimensions(req.Size); ok {
		w, h = dw, dh
	} else if s, ok := ep.ExtraBody["size"].(string); ok {
		if dw, dh, ok := Dimensions(s); ok {
			w, h = dw, dh
		}
	}
	n := req.N
	if n <= 0 {
		n = 1
	}
	fps := 16.0
	if f, ok := ep.ExtraBody["fps"].(float64); ok && f > 0 {
		fps = f
	}
	secs := req.Seconds
	if secs <= 0 {
		secs = 5
	}
	negative, _ := ep.ExtraBody["negative"].(string)
	scale := req.Scale
	if scale <= 0 {
		scale = 2
	}
	quote := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b[1 : len(b)-1])
	}
	r := strings.NewReplacer(
		"{{prompt}}", quote(req.Prompt),
		"{{negative}}", quote(negative),
		"{{width}}", strconv.Itoa(w),
		"{{height}}", strconv.Itoa(h),
		"{{seed}}", strconv.FormatInt(seed, 10),
		"{{batch}}", strconv.Itoa(n),
		"{{seconds}}", strconv.Itoa(secs),
		"{{frames}}", strconv.Itoa(int(float64(secs)*fps)),
		"{{source}}", quote(source),
		"{{mask}}", quote(mask),
		"{{scale}}", strconv.Itoa(scale),
		"{{model}}", quote(req.Model),
	)
	var wf map[string]any
	if err := json.Unmarshal([]byte(r.Replace(tmpl)), &wf); err != nil {
		return nil, fmt.Errorf("comfyui: the filled workflow is not valid JSON: %w", err)
	}
	return wf, nil
}

// upload sends a source image to ComfyUI's input folder and returns the
// name a LoadImage node takes.
func (c *ComfyUI) upload(ctx context.Context, client *http.Client, base string, p *gateway.Provider, src *Output) (string, error) {
	body, ctype, err := form(map[string]string{"overwrite": "true"}, map[string]*Output{"image": src})
	if err != nil {
		return "", err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/upload/image", body)
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", ctype)
	authorize(hreq, p, "Bearer")
	var out struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := doJSON(ctx, client, "comfyui", hreq, &out); err != nil {
		return "", err
	}
	if out.Name == "" {
		return "", errors.New("comfyui: upload returned no file name")
	}
	if out.Subfolder != "" {
		return out.Subfolder + "/" + out.Name, nil
	}
	return out.Name, nil
}

func (c *ComfyUI) seed() int64 {
	if c.Seed != nil {
		return c.Seed()
	}
	return rand.Int64N(1 << 53)
}

// comfyError pulls the exception text out of a history's status messages,
// which are [name, {…}] pairs.
func comfyError(msgs []json.RawMessage) string {
	for _, m := range msgs {
		var pair []json.RawMessage
		if json.Unmarshal(m, &pair) != nil || len(pair) != 2 {
			continue
		}
		var name string
		_ = json.Unmarshal(pair[0], &name)
		if name != "execution_error" {
			continue
		}
		var d struct {
			NodeType string `json:"node_type"`
			Message  string `json:"exception_message"`
		}
		if json.Unmarshal(pair[1], &d) == nil && d.Message != "" {
			if d.NodeType != "" {
				return d.NodeType + ": " + d.Message
			}
			return d.Message
		}
	}
	return "see the ComfyUI log"
}
