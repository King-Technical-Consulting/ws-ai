package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// WorkerClient is serve's handle on the worker's InternalAPI.
type WorkerClient struct {
	BaseURL string // http://worker:8082
	Token   string
	HTTP    *http.Client

	ipMu    sync.Mutex
	ipCache map[uuid.UUID]ipCached
}

type ipCached struct {
	info Info
	at   time.Time
}

// NewWorkerClient builds a client.
func NewWorkerClient(baseURL, token string) *WorkerClient {
	return &WorkerClient{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 11 * time.Minute}, ipCache: map[uuid.UUID]ipCached{}}
}

// WSURL returns the WebSocket URL for a path on the worker.
func (c *WorkerClient) WSURL(path string) string {
	u := c.BaseURL + path
	u = strings.Replace(u, "http://", "ws://", 1)
	u = strings.Replace(u, "https://", "wss://", 1)
	return u
}

func (c *WorkerClient) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(InternalHeader, c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("worker: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return nil, &WorkerError{Status: resp.StatusCode, Msg: e.Error}
	}
	return resp, nil
}

// WorkerError is a non-2xx from the worker.
type WorkerError struct {
	Status int
	Msg    string
}

func (e *WorkerError) Error() string { return fmt.Sprintf("worker: %d %s", e.Status, e.Msg) }

func decodeInto(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// Ensure makes sure the sandbox for (project, user) is running.
func (c *WorkerClient) Ensure(ctx context.Context, projectID, userID uuid.UUID) (Info, error) {
	b, _ := json.Marshal(map[string]any{"project_id": projectID, "user_id": userID})
	resp, err := c.do(ctx, http.MethodPost, "/internal/sandboxes/ensure", bytes.NewReader(b), "application/json")
	if err != nil {
		return Info{}, err
	}
	var info Info
	err = decodeInto(resp, &info)
	c.remember(info)
	return info, err
}

// Get describes a sandbox.
func (c *WorkerClient) Get(ctx context.Context, id uuid.UUID) (Info, error) {
	resp, err := c.do(ctx, http.MethodGet, "/internal/sandboxes/"+id.String(), nil, "")
	if err != nil {
		return Info{}, err
	}
	var info Info
	err = decodeInto(resp, &info)
	c.remember(info)
	return info, err
}

func (c *WorkerClient) remember(info Info) {
	if info.ID == uuid.Nil {
		return
	}
	c.ipMu.Lock()
	c.ipCache[info.ID] = ipCached{info: info, at: time.Now()}
	c.ipMu.Unlock()
}

// Cached returns a recent Info without a round trip, for the preview proxy.
func (c *WorkerClient) Cached(id uuid.UUID, maxAge time.Duration) (Info, bool) {
	c.ipMu.Lock()
	defer c.ipMu.Unlock()
	e, ok := c.ipCache[id]
	if !ok || time.Since(e.at) > maxAge || e.info.IP == "" || e.info.Status != "running" {
		return Info{}, false
	}
	return e.info, true
}

// Stop stops a sandbox.
func (c *WorkerClient) Stop(ctx context.Context, id uuid.UUID) error {
	resp, err := c.do(ctx, http.MethodPost, "/internal/sandboxes/"+id.String()+"/stop", nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Tree lists a directory.
func (c *WorkerClient) Tree(ctx context.Context, id uuid.UUID, path string) (json.RawMessage, error) {
	resp, err := c.do(ctx, http.MethodGet, "/internal/sandboxes/"+id.String()+"/tree?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ReadFile returns a file's bytes and whether the worker flagged it binary.
func (c *WorkerClient) ReadFile(ctx context.Context, id uuid.UUID, path string) ([]byte, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/internal/sandboxes/"+id.String()+"/file?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.Header.Get("X-WS-Binary") == "1", err
}

// WriteFile writes a file.
func (c *WorkerClient) WriteFile(ctx context.Context, id uuid.UUID, path string, data []byte) error {
	resp, err := c.do(ctx, http.MethodPut, "/internal/sandboxes/"+id.String()+"/file?path="+url.QueryEscape(path), bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Healthy pings the worker.
func (c *WorkerClient) Healthy(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/healthz", nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ErrNoWorker is returned by serve when no worker URL is configured.
var ErrNoWorker = errors.New("no worker configured for sandboxes")
