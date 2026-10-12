package deployer

import (
	"strings"
	"testing"
)

func TestParseWindowsExporterConfigDefaults(t *testing.T) {
	config, err := parseWindowsExporterConfig("")
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	joined := strings.Join(config.Collectors, ",")
	for _, required := range []string{"cpu", "logical_disk", "memory", "net", "os", "service", "system"} {
		if !strings.Contains(","+joined+",", ","+required+",") {
			t.Fatalf("default collectors %q do not contain %q", joined, required)
		}
	}
}

func TestParseWindowsExporterConfigRejectsUnsafeValues(t *testing.T) {
	tests := []string{
		`{"collectors":["cpu;Remove-Item C:\\\\*"]}`,
		`{"collectors":["cpu","cpu"]}`,
		`{"collectors":["cpu"],"remote_address":"all"}`,
	}
	for _, raw := range tests {
		if _, err := parseWindowsExporterConfig(raw); err == nil {
			t.Fatalf("unsafe configuration was accepted: %s", raw)
		}
	}
}

func TestParseWindowsExporterConfigAcceptsScopedFirewall(t *testing.T) {
	config, err := parseWindowsExporterConfig(`{"collectors":["cpu","memory"],"remote_address":"10.20.0.0/16"}`)
	if err != nil {
		t.Fatalf("parse scoped firewall configuration: %v", err)
	}
	if config.RemoteAddress != "10.20.0.0/16" {
		t.Fatalf("remote address=%q", config.RemoteAddress)
	}
}

func TestQuotePowerShellEscapesSingleQuote(t *testing.T) {
	if got, want := quotePowerShell(`C:\\Temp\\operator's.msi`), `'C:\\Temp\\operator''s.msi'`; got != want {
		t.Fatalf("quotePowerShell()=%q, want %q", got, want)
	}
}
