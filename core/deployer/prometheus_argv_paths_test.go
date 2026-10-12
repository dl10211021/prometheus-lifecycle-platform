package deployer

import "testing"

func TestPrometheusArgvPaths(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		config, data string
		reject       bool
	}{
		{"equals", []string{"prometheus", "--config.file=/srv/prom/prometheus.yml", "--storage.tsdb.path=/srv/prom/data"}, "/srv/prom/prometheus.yml", "/srv/prom/data", false},
		{"separate", []string{"prometheus", "--config.file", "/srv/prom/prometheus.yml", "--storage.tsdb.path", "/srv/prom/data"}, "/srv/prom/prometheus.yml", "/srv/prom/data", false},
		{"relative-default", []string{"--config.file=prometheus.yml", "--storage.tsdb.path=data"}, "", "", true},
		{"missing-data", []string{"--config.file=/srv/prom/prometheus.yml"}, "", "", true},
		{"duplicate", []string{"--config.file=/srv/a", "--config.file=/srv/b", "--storage.tsdb.path=/srv/data"}, "", "", true},
		{"agent", []string{"--config.file=/srv/c", "--storage.tsdb.path=/srv/d", "--agent"}, "", "", true},
		{"feature-agent", []string{"--config.file=/srv/c", "--storage.tsdb.path=/srv/d", "--enable-feature", "agent"}, "", "", true},
		{"private-tmp", []string{"--config.file=/srv/c", "--storage.tsdb.path=/tmp/d"}, "", "", true},
		{"variable", []string{"--config.file=/srv/c", "--storage.tsdb.path=$DATA"}, "", "", true},
		{"traversal", []string{"--config.file=/srv/c", "--storage.tsdb.path=/srv/../d"}, "", "", true},
		{"terminator", []string{"--config.file=/srv/c", "--storage.tsdb.path=/srv/d", "--"}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, data, err := PrometheusArgvPaths(tc.args)
			if tc.reject {
				if err == nil {
					t.Fatal("unsafe observed argv admitted")
				}
				return
			}
			if err != nil || config != tc.config || data != tc.data {
				t.Fatalf("paths=%q %q error=%v", config, data, err)
			}
		})
	}
}
