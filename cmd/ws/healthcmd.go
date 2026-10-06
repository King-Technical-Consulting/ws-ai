package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jking323/ws/internal/config"
)

// healthcheck probes the serve process's two listeners. It exists so the
// distroless container can run a healthcheck without a shell.
func healthcheck(cfg *config.Config) int {
	client := &http.Client{Timeout: 3 * time.Second}
	for _, addr := range []string{cfg.Listen, cfg.ArtifactListen} {
		host := addr
		if strings.HasPrefix(host, ":") {
			host = "127.0.0.1" + host
		}
		resp, err := client.Get("http://" + host + "/healthz")
		if err != nil {
			fmt.Fprintf(os.Stderr, "unhealthy: %s: %v\n", addr, err)
			return 1
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, "unhealthy: %s: HTTP %d\n", addr, resp.StatusCode)
			return 1
		}
	}
	return 0
}
