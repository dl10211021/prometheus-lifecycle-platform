package utils

import "testing"

func TestSafeInstallPath(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"",
		"/opt/promlifecycle/components/prometheus",
		"/usr/local/promlifecycle/prometheus",
		"/srv/monitoring/prometheus",
		"/tmp/prometheus-e2e",
	} {
		if !SafeInstallPath(path) {
			t.Errorf("expected safe install path: %q", path)
		}
	}
	for _, path := range []string{
		"/", "/opt", "/tmp", "/opt/../etc", "/usr", "/usr/share/grafana", "/var/lib/prometheus",
		"/etc/prometheus", "/root/prometheus", "/opt/prom lifecycle", `/opt/prom"bad`,
	} {
		if SafeInstallPath(path) {
			t.Errorf("expected unsafe install path: %q", path)
		}
	}
}

func TestSafeDataPath(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"",
		"/var/lib/prometheus",
		"/var/lib/promlifecycle/prometheus",
		"/data/prometheus",
		"/srv/monitoring/data",
		"/opt/promlifecycle/data/prometheus",
		"/tmp/prometheus-data-e2e",
	} {
		if !SafeDataPath(path) {
			t.Errorf("expected safe data path: %q", path)
		}
	}
	for _, path := range []string{
		"/", "/data", "/opt", "/var", "/var/lib", "/var/log/prometheus", "/etc/prometheus",
		"/usr/local/data", "/home/prometheus", "/data/../etc", "/data/prom data",
	} {
		if SafeDataPath(path) {
			t.Errorf("expected unsafe data path: %q", path)
		}
	}
}
