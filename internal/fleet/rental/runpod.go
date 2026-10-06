package rental

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// RunPod rents pods through RunPod's REST API (https://rest.runpod.io/v1,
// bearer key). A pod exposes its HTTP port through RunPod's proxy at
// https://<pod id>-<port>.proxy.runpod.net, which is what the endpoint
// uses; the server behind it still requires the rental key.
type RunPod struct {
	APIKey  string
	BaseURL string // default https://rest.runpod.io/v1
	Client  *http.Client
}

// Name implements Provider.
func (r *RunPod) Name() string { return "runpod" }

func (r *RunPod) base() string {
	if r.BaseURL != "" {
		return strings.TrimRight(r.BaseURL, "/")
	}
	return "https://rest.runpod.io/v1"
}

func (r *RunPod) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (r *RunPod) do(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return fmt.Errorf("runpod: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("runpod: %s %s: HTTP %d: %s", method, path, resp.StatusCode, apiMessage(rb))
	}
	if out != nil && len(rb) > 0 {
		if err := json.Unmarshal(rb, out); err != nil {
			return fmt.Errorf("runpod: decode %s: %w", path, err)
		}
	}
	return nil
}

func apiMessage(b []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Error != "" {
			return e.Error
		}
		if e.Message != "" {
			return e.Message
		}
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// runpodGPUType is one entry of GET /gputypes.
type runpodGPUType struct {
	ID             string  `json:"id"`
	DisplayName    string  `json:"displayName"`
	MemoryInGb     int     `json:"memoryInGb"`
	SecureCloud    bool    `json:"secureCloud"`
	CommunityCloud bool    `json:"communityCloud"`
	SecurePrice    float64 `json:"securePrice"`
	CommunityPrice float64 `json:"communityPrice"`
	// Older shapes carry prices under lowestPrice.
	LowestPrice *struct {
		UninterruptablePrice float64 `json:"uninterruptablePrice"`
		MinimumBidPrice      float64 `json:"minimumBidPrice"`
	} `json:"lowestPrice"`
}

// Offers implements Provider.
func (r *RunPod) Offers(ctx context.Context) ([]Offer, error) {
	var types []runpodGPUType
	if err := r.do(ctx, http.MethodGet, "/gputypes", nil, &types); err != nil {
		return nil, err
	}
	out := make([]Offer, 0, len(types))
	for _, t := range types {
		price := t.SecurePrice
		if price == 0 && t.LowestPrice != nil {
			price = t.LowestPrice.UninterruptablePrice
		}
		if price == 0 {
			price = t.CommunityPrice
		}
		out = append(out, Offer{GPU: t.ID, MemoryGB: t.MemoryInGb, HourlyUSD: price, Available: price > 0 && (t.SecureCloud || t.CommunityCloud)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GPU < out[j].GPU })
	return out, nil
}

// runpodPod is the pod object of POST/GET /pods.
type runpodPod struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	DesiredStatus string            `json:"desiredStatus"` // RUNNING | EXITED | TERMINATED
	CostPerHr     float64           `json:"costPerHr"`
	PortMappings  map[string]int    `json:"portMappings"` // "8000" -> public port, when direct
	PublicIP      string            `json:"publicIp"`
	Runtime       *runpodRuntime    `json:"runtime"`
	LastError     string            `json:"lastError"`
	Env           map[string]string `json:"env"`
}

type runpodRuntime struct {
	UptimeInSeconds int `json:"uptimeInSeconds"`
	Ports           []struct {
		IP          string `json:"ip"`
		IsIPPublic  bool   `json:"isIpPublic"`
		PrivatePort int    `json:"privatePort"`
		PublicPort  int    `json:"publicPort"`
		Type        string `json:"type"`
	} `json:"ports"`
}

// Start implements Provider: POST /pods.
func (r *RunPod) Start(ctx context.Context, spec StartSpec) (string, error) {
	t := spec.Template
	cloud := "SECURE"
	if strings.EqualFold(t.CloudType, "community") {
		cloud = "COMMUNITY"
	}
	body := map[string]any{
		"name":              spec.Name,
		"imageName":         t.Image,
		"gpuTypeIds":        []string{t.GPU},
		"gpuCount":          t.GPUCount,
		"cloudType":         cloud,
		"containerDiskInGb": t.DiskGB,
		"volumeInGb":        t.VolumeGB,
		"volumeMountPath":   t.VolumePath,
		"ports":             []string{fmt.Sprintf("%d/http", t.Port)},
		"env":               spec.Env,
	}
	if strings.TrimSpace(spec.Args) != "" {
		body["dockerStartCmd"] = strings.Fields(spec.Args)
	}
	var pod runpodPod
	if err := r.do(ctx, http.MethodPost, "/pods", body, &pod); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no longer any instances available") || strings.Contains(strings.ToLower(err.Error()), "not available") {
			return "", fmt.Errorf("%w: %v", ErrNotAvailable, err)
		}
		return "", err
	}
	if pod.ID == "" {
		return "", errors.New("runpod: pod created without an id")
	}
	return pod.ID, nil
}

// Status implements Provider: GET /pods/{id}.
func (r *RunPod) Status(ctx context.Context, id string) (Status, error) {
	var pod runpodPod
	if err := r.do(ctx, http.MethodGet, "/pods/"+id, nil, &pod); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return Status{State: "exited", Error: "pod not found"}, nil
		}
		return Status{}, err
	}
	st := Status{HourlyUSD: pod.CostPerHr, Error: pod.LastError}
	switch strings.ToUpper(pod.DesiredStatus) {
	case "EXITED", "TERMINATED":
		st.State = "exited"
		return st, nil
	case "RUNNING":
		st.State = "starting"
	default:
		st.State = "unknown"
	}
	// The HTTP port is reachable through RunPod's proxy once the runtime
	// reports it; the proxy host is derived from the pod id.
	if pod.Runtime != nil {
		for _, p := range pod.Runtime.Ports {
			if strings.EqualFold(p.Type, "http") {
				st.State = "running"
				st.BaseURL = fmt.Sprintf("https://%s-%d.proxy.runpod.net/v1", pod.ID, p.PrivatePort)
				break
			}
		}
	}
	return st, nil
}

// Stop implements Provider: DELETE /pods/{id} terminates the pod.
func (r *RunPod) Stop(ctx context.Context, id string) error {
	err := r.do(ctx, http.MethodDelete, "/pods/"+id, nil, nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return nil
	}
	return err
}
