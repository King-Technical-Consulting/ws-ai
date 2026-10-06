package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/jking323/ws/internal/gateway"
)

// Shared HTTP plumbing for the engines: one client with a long timeout,
// provider auth, bounded reads, and a multipart builder for the engines
// that upload a source image.

func defaultClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// authorize sets the provider's key and extra headers. scheme is the
// Authorization scheme ("Bearer" for OpenAI, "Key" for fal).
func authorize(req *http.Request, p *gateway.Provider, scheme string) {
	if p == nil {
		return
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", scheme+" "+p.APIKey)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
}

// readBody reads at most max bytes and reports when the body was longer.
func readBody(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("response larger than %d MB", max>>20)
	}
	return b, nil
}

// doJSON sends a request and decodes a JSON reply, with the engine's name
// in every error.
func doJSON(ctx context.Context, c *http.Client, engine string, req *http.Request, out any) error {
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("%s: %w", engine, err)
	}
	defer resp.Body.Close()
	b, err := readBody(resp.Body, 8<<20)
	if err != nil {
		return fmt.Errorf("%s: read: %w", engine, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: HTTP %d: %s", engine, resp.StatusCode, apiError(b))
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: decode: %w", engine, err)
	}
	return nil
}

// fetchBytes downloads a generated file (an image or a video) of at most
// max bytes.
func fetchBytes(ctx context.Context, c *http.Client, engine, url string, p *gateway.Provider, scheme string, max int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	authorize(req, p, scheme)
	return fetchRequest(c, engine, req, max)
}

// fetchRequest is fetchBytes for a request the caller authorized itself.
func fetchRequest(c *http.Client, engine string, req *http.Request, max int64) ([]byte, string, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%s: fetch output: %w", engine, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := readBody(resp.Body, 4096)
		return nil, "", fmt.Errorf("%s: fetch output: HTTP %d: %s", engine, resp.StatusCode, apiError(b))
	}
	b, err := readBody(resp.Body, max)
	if err != nil {
		return nil, "", fmt.Errorf("%s: fetch output: %w", engine, err)
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// form builds a multipart body: string fields plus files {field: output}.
func form(fields map[string]string, files map[string]*Output) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	for field, o := range files {
		if o == nil {
			continue
		}
		name := "source" + extFor(o.MIME)
		if name == "source" {
			name = "source.bin"
		}
		fw, err := w.CreateFormFile(field, name)
		if err != nil {
			return nil, "", err
		}
		if _, err := fw.Write(o.Data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

// dataURL inlines an image for APIs that take a URL.
func dataURL(o *Output) string {
	mime := o.MIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64Encode(o.Data)
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// outputMIME picks a MIME type: the declared one when it is specific,
// else what the bytes look like.
func outputMIME(declared string, data []byte) string {
	declared = strings.TrimSpace(strings.Split(declared, ";")[0])
	if declared != "" && declared != "application/octet-stream" && declared != "binary/octet-stream" {
		return declared
	}
	return http.DetectContentType(data)
}

var errNoOutput = errors.New("the engine returned no output")
