package deployer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/promlifecycle/platform/models"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in probes of the generated candidate using verified official binaries.
// A deliberately invalid external URL stops after argument value parsing,
// before configuration, listeners or TSDB initialization even on old releases.
// --version cannot test duration values: kingpin exits before value validation.
func TestRealPrometheusParameterMigrationCLI(t *testing.T) {
	manifest := os.Getenv("PROMLIFECYCLE_PARAMETER_CLI_BINARIES")
	if manifest == "" {
		t.Skip("set PROMLIFECYCLE_PARAMETER_CLI_BINARIES to verified binary inventory")
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var binaries map[string]struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(raw, &binaries); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2.6.1", "2.7.0", "2.55.0", "3.1.0"} {
		t.Run(version, func(t *testing.T) {
			binary, ok := binaries[version]
			if !ok || !filepath.IsAbs(binary.Path) {
				t.Fatal("missing absolute binary path")
			}
			content, err := os.ReadFile(binary.Path)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(content)
			if hex.EncodeToString(hash[:]) != binary.SHA256 {
				t.Fatal("binary checksum mismatch")
			}
			probeRoot := t.TempDir()
			missingConfig := filepath.Join(probeRoot, "missing.yml")
			native := func(t *testing.T, args []string, want bool) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				flags := []string{"--web.external-url=://parameter-probe-invalid", "--config.file=" + missingConfig, "--storage.tsdb.path=" + filepath.Join(probeRoot, "unexpected-tsdb"), "--web.listen-address=127.0.0.1:0"}
				command := exec.CommandContext(ctx, binary.Path, append(flags, args...)...)
				command.Dir = probeRoot
				output, err := command.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() == 0 {
					t.Fatalf("probe did not stop: %v %s", err, output)
				}
				reachedURL := strings.Contains(string(output), "parse external URL") && strings.Contains(string(output), "://parameter-probe-invalid")
				if reachedURL != want {
					t.Fatalf("args=%v parsed=%v want=%v: %s", args, reachedURL, want, output)
				}
				if !want && !strings.Contains(strings.ToLower(string(output)), "error parsing command") {
					t.Fatalf("unexpected rejection: %s", output)
				}
				entries, err := os.ReadDir(probeRoot)
				if err != nil || len(entries) != 0 {
					t.Fatalf("probe created runtime files: %v %v", entries, err)
				}
			}
			t.Run("unknown-canary", func(t *testing.T) { native(t, []string{"--definitely-not-a-prometheus-option"}, false) })
			for _, value := range []string{"0", "0s", "045d", "1d2h3ms", "292y", "9223372036854ms"} {
				t.Run(value, func(t *testing.T) {
					// 2.6 has the old flag; all later versions receive actual preview output.
					for _, args := range [][]string{{"--storage.tsdb.retention=" + value}, {"--storage.tsdb.retention", value}} {
						p := models.PreviewPrometheusArguments("2.55.0", version, args)
						oldDurationBoundary := (version == "2.6.1" || version == "2.7.0") && (value == "0" || value == "0s" || value == "1d2h3ms")
						if oldDurationBoundary {
							if p.Status != "blocked" {
								t.Fatalf("unverified old duration offered: %+v", p)
							}
							nativeArgs := append([]string(nil), args...)
							if version == "2.7.0" {
								nativeArgs[0] = strings.Replace(nativeArgs[0], "--storage.tsdb.retention", "--storage.tsdb.retention.time", 1)
							}
							// Old parsers accept 0s, but its cross-version meaning
							// is deliberately outside the automatic proposal.
							native(t, nativeArgs, value == "0s")
						} else {
							if p.Status == "blocked" {
								t.Fatalf("no candidate: %+v", p)
							}
							native(t, p.Arguments, true)
						}
					}
				})
			}
			if version == "3.1.0" {
				t.Run("old-alias-rejected", func(t *testing.T) { native(t, []string{"--storage.tsdb.retention=045d"}, false) })
			}
			if version == "2.55.0" || version == "3.1.0" {
				flag := "--storage.tsdb.retention="
				if version == "3.1.0" {
					flag = "--storage.tsdb.retention.time="
				}
				for _, value := range []string{"-1d", "1.5h", "1h1d", "1m1m", "293y", "9223372036855ms", "18446744073709551616ms"} {
					t.Run("invalid-"+value, func(t *testing.T) {
						p := models.PreviewPrometheusArguments("2.55.0", "3.1.0", []string{"--storage.tsdb.retention=" + value})
						if p.Status != "blocked" {
							t.Fatal("invalid duration offered for migration")
						}
						native(t, []string{flag + value}, false)
					})
				}
			}
		})
	}
}
