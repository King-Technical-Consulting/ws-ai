package chat

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// decodeDataURL extracts bytes from a data: URL (base64 only).
func decodeDataURL(u string) ([]byte, error) {
	i := strings.Index(u, ",")
	if i < 0 {
		return nil, fmt.Errorf("bad data url")
	}
	meta, payload := u[5:i], u[i+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return nil, fmt.Errorf("only base64 data urls supported")
	}
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("bad data url: %w", err)
	}
	return b, nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
