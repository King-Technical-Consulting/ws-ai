package docread

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// HTTPOCR talks to the OCR container (infra/ocr): POST /ocr with the file as
// multipart field "file" and `max_pages`, answering {"pages":[{"n":1,"text":""}]}.
// The container is a black box; only this contract matters.
type HTTPOCR struct {
	URL    string
	Client *http.Client
}

// MaxOCRResponse bounds what a misbehaving container can send back.
const MaxOCRResponse = 32 << 20

// Recognize implements OCR.
func (o *HTTPOCR) Recognize(ctx context.Context, mime, name string, data []byte, maxPages int) ([]Page, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, safeName(name)))
	h.Set("Content-Type", mime)
	fw, err := mw.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, err
	}
	_ = mw.WriteField("max_pages", fmt.Sprint(maxPages))
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.URL, "/")+"/ocr", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c := o.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ocr service unreachable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxOCRResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ocr service answered %d", resp.StatusCode)
	}
	var out struct {
		Pages []Page `json:"pages"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("ocr service answered something else")
	}
	for i := range out.Pages {
		if out.Pages[i].N == 0 {
			out.Pages[i].N = i + 1
		}
	}
	return out.Pages, nil
}

func safeName(n string) string {
	n = strings.Map(func(r rune) rune {
		if r < 32 || r == '"' || r == '\\' || r == '/' {
			return '_'
		}
		return r
	}, n)
	if n == "" {
		return "file"
	}
	return n
}
