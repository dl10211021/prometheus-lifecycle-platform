package deployer

import (
	"encoding/json"
	"github.com/promlifecycle/platform/models"
	"testing"
)

func TestPrometheusRuntimeRecoveryPathsUseEffectiveArguments(t *testing.T) {
	for _, mode := range []string{"defaults", "equals", "split", "custom path locked", "unrelated flags", "default-path replacement", "docker defaults", "docker unrelated mount"} {
		t.Run(mode, func(t *testing.T) {
			c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", ConfigPath: "/srv/prometheus/prom.yml", DataPath: "/srv/prometheus/data"}
			switch mode {
			case "equals":
				c.ImportedArgs = `["--config.file=/srv/prometheus/prom.yml","--storage.tsdb.path=/srv/prometheus/data"]`
			case "split":
				c.ImportedArgs = `["--config.file","/srv/prometheus/prom.yml","--storage.tsdb.path","/srv/prometheus/data"]`
			case "custom path locked":
				c.CustomArgs = "--config.file=/unbacked/prom.yml\n--storage.tsdb.path=/unbacked/data"
			case "unrelated flags":
				c.CustomArgs = "--query.timeout=90s"
			case "default-path replacement":
				spec, _ := binarySpecFor(c.Type)
				args, _ := json.Marshal([]string{"--config.file=" + c.ConfigPath, "--storage.tsdb.path=" + spec.DataDir})
				c.ImportedArgs = string(args)
			case "docker defaults":
				c.DeployType = "docker"
			case "docker unrelated mount":
				c.DeployType = "docker"
				c.ExtraVolumes = `[{"src":"/srv/certs","dst":"/certs"}]`
			}
			config, data, err := PrometheusRuntimePaths(&c)
			if err != nil || config != c.ConfigPath || data != c.DataPath {
				t.Fatalf("effective scope mismatch: %s %s %v", config, data, err)
			}
		})
	}
	for _, argv := range []string{`["--config.file=/a","--config.file=/b","--storage.tsdb.path=/data"]`, `["--config.file"]`, `["--config.file=/a","--storage.tsdb.path=relative"]`, `not-json`} {
		c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", ImportedArgs: argv}
		if _, _, err := PrometheusRuntimePaths(&c); err == nil {
			t.Errorf("ambiguous argv scope accepted")
		}
	}
}

func TestPrometheusRuntimeRecoveryScopeRejectsDynamicAndAgentStorage(t *testing.T) {
	for _, mode := range []string{"systemd variable", "agent", "agent feature", "docker colon mount", "docker nested shadow"} {
		t.Run(mode, func(t *testing.T) {
			c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", ConfigPath: "/srv/prom.yml", DataPath: "/srv/data"}
			switch mode {
			case "systemd variable":
				c.ConfigPath = "/srv/${CFG}/prom.yml"
				c.CustomEnv = `{"CFG":"actual"}`
			case "agent":
				c.CustomArgs = "--agent"
			case "agent feature":
				c.CustomArgs = "--enable-feature=agent"
			case "docker colon mount":
				c.DeployType = "docker"
				c.ExtraVolumes = `[{"src":"/outside","dst":"/var/lib/prometheus:ro"}]`
			case "docker nested shadow":
				c.DeployType = "docker"
				c.ExtraVolumes = `[{"src":"/outside","dst":"/etc/prometheus/prometheus.yml"}]`
			}
			if _, _, err := PrometheusRuntimePaths(&c); err == nil {
				t.Fatal("unproven runtime scope accepted")
			}
		})
	}
}

func TestPrometheusRuntimeRecoveryScopeRejectsPrivateOrTraversalRoots(t *testing.T) {
	for _, path := range []string{"/tmp/prometheus/data", "/var/tmp/prometheus/data", "/srv/link/../data"} {
		c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", ConfigPath: "/etc/prom.yml", DataPath: path}
		if _, _, err := PrometheusRuntimePaths(&c); err == nil {
			t.Errorf("unproven root accepted: %s", path)
		}
	}
}
