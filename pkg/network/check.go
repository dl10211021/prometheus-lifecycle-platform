// Package network provides a lightweight outbound connectivity probe so the
// platform can fail fast (instead of waiting on long timeouts) when it runs in
// a fully offline environment. Set PROMLIFECYCLE_OFFLINE=1 to force offline.
package network

import (
	"net"
	"os"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	cached  bool
	checked time.Time
)

// Available reports whether outbound network seems reachable. The result is
// cached for a short window to avoid probing on every request.
func Available() bool {
	if os.Getenv("PROMLIFECYCLE_OFFLINE") == "1" {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	if time.Since(checked) < 60*time.Second {
		return cached
	}
	cached = probe()
	checked = time.Now()
	return cached
}

// probe tries a short TCP connect to a few well-known public endpoints.
func probe() bool {
	targets := []string{"hub.docker.com:443", "github.com:443", "dl.grafana.com:443"}
	for _, t := range targets {
		conn, err := net.DialTimeout("tcp", t, 3*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}
