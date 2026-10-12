package deployer

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/promlifecycle/platform/models"
)

// PrometheusRuntimePaths resolves only the managed configuration/data roots
// used by a version restore. It shares the real binary argv compiler, including
// imported argv, default-path substitution and locked custom flags. No argv or
// secrets are returned in errors. This is path binding, not format validation.
// systemd expands dollar variables in ExecStart, so unresolved binary paths
// containing dollars cannot be bound to literal archive roots.
func PrometheusRuntimePaths(c *models.Component) (string, string, error) {
	unsupported := func() (string, string, error) {
		return "", "", errors.New("unsupported-scope: cannot bind effective Prometheus configuration/data paths to the full backup")
	}
	if c == nil || c.Type != models.ComponentPrometheus {
		return unsupported()
	}
	switch c.DeployType {
	case "binary":
		spec, err := binarySpecForComponent(c)
		if err != nil {
			return unsupported()
		}
		args := effectiveArgs(spec, c)
		return PrometheusArgvPaths(args)
	case "docker", "":
		// dockerRunArgs mounts these two host roots, while getCommand uses their
		// container counterparts. Additional overlapping mounts would mask data
		// outside the full archive. Unrelated mounts do not change these roots.
		if strings.ContainsAny(c.ConfigPath, ":\x00\r\n") || strings.ContainsAny(c.DataPath, ":\x00\r\n") {
			return unsupported()
		}
		if c.ExtraVolumes != "" {
			var volumes []struct {
				Src string `json:"src"`
				Dst string `json:"dst"`
			}
			if json.Unmarshal([]byte(c.ExtraVolumes), &volumes) != nil {
				return unsupported()
			}
			for _, volume := range volumes {
				if volume.Src == "" || volume.Dst == "" {
					continue
				}
				if strings.ContainsAny(volume.Src, ":\x00\r\n") || strings.ContainsAny(volume.Dst, ":\x00\r\n") {
					return unsupported()
				}
				dst := filepath.Clean(volume.Dst)
				if !filepath.IsAbs(dst) {
					return unsupported()
				}
				for _, owned := range []string{containerConfigPath(c.Type), containerDataPath(c.Type)} {
					if dst == "/" || dst == owned || strings.HasPrefix(dst, owned+"/") || strings.HasPrefix(owned, dst+"/") {
						return unsupported()
					}
				}
			}
		}
		return filepath.Clean(c.ConfigPath), filepath.Clean(c.DataPath), nil
	default:
		// BackupService cannot quiesce Windows or arbitrary deployment backends
		// with its stopped-application full protocol.
		return unsupported()
	}
}

// PrometheusArgvPaths parses observed process argv without applying defaults or
// rewriting values. Relative default paths cannot prove host archive identity.
func PrometheusArgvPaths(args []string) (string, string, error) {
	unsupported := func() (string, string, error) {
		return "", "", errors.New("unsupported-scope: cannot bind effective Prometheus configuration/data paths to the full backup")
	}
	paths := map[string]string{}
	for i := 0; i < len(args); i++ {
		key, value, equals := strings.Cut(args[i], "=")
		// Agent mode writes another data tree. This scoped recovery protocol
		// intentionally supports Prometheus server TSDB only.
		if key == "--agent" && (!equals || value != "false") {
			return unsupported()
		}
		if key == "--enable-feature" {
			features := value
			if !equals && i+1 < len(args) {
				features = args[i+1]
			}
			for _, feature := range strings.Split(features, ",") {
				if strings.TrimSpace(feature) == "agent" {
					return unsupported()
				}
			}
		}
		if args[i] == "--" {
			return unsupported()
		}
		if key != "--config.file" && key != "--storage.tsdb.path" {
			continue
		}
		if _, duplicate := paths[key]; duplicate {
			return unsupported()
		}
		if !equals {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return unsupported()
			}
			i++
			value = args[i]
		}
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "$\x00\r\n") {
			return unsupported()
		}
		for _, part := range strings.Split(value, string(filepath.Separator)) {
			if part == ".." {
				return unsupported()
			}
		}
		clean := filepath.Clean(value)
		for _, private := range []string{"/tmp", "/var/tmp"} {
			if clean == private || strings.HasPrefix(clean, private+"/") {
				return unsupported()
			}
		}
		paths[key] = clean
	}
	if paths["--config.file"] == "" || paths["--storage.tsdb.path"] == "" {
		return unsupported()
	}
	return paths["--config.file"], paths["--storage.tsdb.path"], nil
}
