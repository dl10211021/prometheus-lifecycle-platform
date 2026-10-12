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

// Opt-in parser acceptance only: --version exits before configuration/storage/
// listener initialization. Archive provenance is supplied by the operator's
// verified fixture inventory; the executable hash is rechecked here.
func TestRealPrometheusParameterVersionCLI(t *testing.T) {
	manifestPath := os.Getenv("PROMLIFECYCLE_PARAMETER_CLI_BINARIES")
	if manifestPath == "" {
		t.Skip("set PROMLIFECYCLE_PARAMETER_CLI_BINARIES to a verified binary inventory")
	}
	raw, err := os.ReadFile(manifestPath)
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
	for _, version := range []struct {
		name                    string
		legacy, time, size, wal bool
	}{
		{"2.6.1", true, false, false, false},
		{"2.7.0", true, true, true, false},
		{"2.11.0", true, true, true, true},
		{"2.55.0", true, true, true, true},
		{"3.1.0", false, true, true, true},
	} {
		t.Run(version.name, func(t *testing.T) {
			binary, ok := binaries[version.name]
			if !ok || !filepath.IsAbs(binary.Path) {
				t.Fatal("missing absolute binary path")
			}
			content, err := os.ReadFile(binary.Path)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			if hex.EncodeToString(digest[:]) != binary.SHA256 {
				t.Fatal("binary checksum mismatch")
			}
			native := func(args []string, want bool) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				output, err := exec.CommandContext(ctx, binary.Path, append(append([]string{}, args...), "--version")...).CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("CLI probe timed out: %v", ctx.Err())
				}
				if want {
					if err != nil || !strings.Contains(string(output), "version "+version.name) {
						t.Fatalf("CLI rejected %v: %v %s", args, err, output)
					}
				} else {
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() == 0 {
						t.Fatalf("CLI did not reject %v: %v %s", args, err, output)
					}
					if !strings.Contains(string(output), "unknown long flag") {
						t.Fatalf("wrong native rejection for %v: %s", args, output)
					}
				}
			}
			t.Run("unknown-option-canary", func(t *testing.T) { native([]string{"--definitely-not-a-prometheus-option"}, false) })
			for _, tc := range []struct {
				name string
				args []string
				want bool
			}{
				{"legacy", []string{"--storage.tsdb.retention=45d"}, version.legacy},
				{"time", []string{"--storage.tsdb.retention.time=45d"}, version.time},
				{"size", []string{"--storage.tsdb.retention.size=50GB"}, version.size},
				{"wal", []string{"--storage.tsdb.wal-compression"}, version.wal},
				{"no-wal", []string{"--no-storage.tsdb.wal-compression"}, version.wal},
			} {
				t.Run(tc.name, func(t *testing.T) {
					native(tc.args, tc.want)
					err := models.ValidatePrometheusArgumentVersion(version.name, tc.args)
					if (err == nil) != tc.want {
						t.Fatalf("policy disagrees with native boundary: %v", err)
					}
				})
			}
			t.Run("platform-defaults", func(t *testing.T) {
				component := &models.Component{Type: models.ComponentPrometheus, DeployType: "binary", Version: version.name}
				spec, err := binarySpecForComponent(component)
				if err != nil {
					t.Fatal(err)
				}
				native(effectiveArgs(spec, component), version.wal)
				if err := ValidatePrometheusParameterVersion(component, version.name); (err == nil) != version.wal {
					t.Fatalf("default admission disagrees with native parser: %v", err)
				}
			})
		})
	}
}
