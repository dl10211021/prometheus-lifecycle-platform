package handlers

import (
	"context"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"strings"
	"testing"
)

func TestRemoteManagedAdoptionRejectsUnmanageablePrometheusPaths(t *testing.T) {
	cases := []struct {
		name, config, data string
		extra              []string
		reason             string
	}{
		{name: "temporary configuration", config: "/tmp/prometheus.yml", data: "/srv/prom/data", reason: "生命周期管理"},
		{name: "relative configuration", config: "prometheus.yml", data: "/srv/prom/data", reason: "生命周期管理"},
		{name: "agent mode", config: "/etc/prometheus/prometheus.yml", data: "/srv/prom/data", extra: []string{"--agent"}, reason: "生命周期管理"},
		{name: "duplicate path", config: "/etc/prometheus/prometheus.yml", data: "/srv/prom/data", extra: []string{"--config.file=/etc/other.yml"}, reason: "生命周期管理"},
		{name: "overlap", config: "/srv/prom/data/prometheus.yml", data: "/srv/prom/data", reason: "互不重叠"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected := &hostProcessEvidence{component: services.DiscoveredComponent{Type: "prometheus", Binary: "/srv/prom/prometheus", Config: tc.config, Data: tc.data, Unit: "existing.service"}, args: append([]string{"--config.file=" + tc.config, "--storage.tsdb.path=" + tc.data}, tc.extra...)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := probeRemoteTakeover(ctx, &models.Host{IP: "127.0.0.2", Port: 22, Username: "root", Password: "fixture"}, selected)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("unmanageable original runtime admitted or connected: %v", err)
			}
		})
	}
}
