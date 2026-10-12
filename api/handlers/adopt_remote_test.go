package handlers

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	gossh "golang.org/x/crypto/ssh"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Only the remote machine is substituted; SSH, discovery, handler, DB and
// archive writing remain real. No fixture executes local service commands.
func adoptionSSH(t *testing.T, reply func(string) (string, uint32)) int {
	t.Helper()
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "insecure")
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &gossh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
				server, channels, requests, err := gossh.NewServerConn(raw, config)
				if err != nil {
					return
				}
				defer server.Close()
				go gossh.DiscardRequests(requests)
				for incoming := range channels {
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					for request := range requests {
						if request.Type != "exec" {
							_ = request.Reply(false, nil)
							continue
						}
						var execRequest struct{ Command string }
						if gossh.Unmarshal(request.Payload, &execRequest) != nil {
							_ = request.Reply(false, nil)
							continue
						}
						_ = request.Reply(true, nil)
						out, code := reply(execRequest.Command)
						_, _ = io.WriteString(channel, out)
						_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{code}))
						_ = channel.Close()
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done; wg.Wait() })
	return listener.Addr().(*net.TCPAddr).Port
}

func TestRemoteManagedAdoptionPreservesOriginalConfigAndUnit(t *testing.T) {
	for _, scenario := range []string{"success", "preflight_rejected", "database_failure", "changed_launch", "health_unreachable"} {
		t.Run(scenario, func(t *testing.T) {
			reject := scenario == "preflight_rejected"
			gin.SetMode(gin.TestMode)
			root := t.TempDir()
			configPath := "/srv/existing/config/existing.yml"
			db, err := gorm.Open(sqlite.Open(filepath.Join(root, "test.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err = db.AutoMigrate(&models.Host{}, &models.Cluster{}, &models.Component{}, &models.ComponentAdoption{}, &models.ComponentConfig{}, &models.HostOperationClaim{}, &models.OperationLog{}); err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { _ = sqlDB.Close() })
			healthListener, err := net.Listen("tcp", "127.0.0.2:0")
			if err != nil {
				t.Fatal(err)
			}
			healthPort := healthListener.Addr().(*net.TCPAddr).Port
			healthServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })}
			if scenario == "health_unreachable" {
				healthListener.Close()
			} else {
				go healthServer.Serve(healthListener)
				t.Cleanup(func() { healthServer.Close() })
			}
			const remoteConfig = "global:\n  scrape_interval: 31s\nscrape_configs: []\n"
			const originalUnit = "[Service]\nExecStart=/srv/existing/prometheus --config.file=/srv/existing/prometheus.yml\n"
			encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
			args := "prometheus\x00--config.file=" + configPath + "\x00--storage.tsdb.path=/srv/existing/data\x00"
			stat := "531 (prometheus) S " + strings.Repeat("0 ", 18) + "12345"
			discovery := "LISTENERS\nLISTEN 0 4096 0.0.0.0:" + strconv.Itoa(healthPort) + " 0.0.0.0:* users:((\"prometheus\",pid=531,fd=7))\nPROCESSES\nPROC\t531\t/srv/existing/prometheus\t" + encode(args) + "\t" + encode("0::/system.slice/existing-prometheus.service\n") + "\t" + encode(stat) + "\n"
			var commandsMu sync.Mutex
			var commands []string
			port := adoptionSSH(t, func(command string) (string, uint32) {
				commandsMu.Lock()
				commands = append(commands, command)
				commandsMu.Unlock()
				switch {
				case command == hostDiscoveryCommand:
					return discovery, 0
				case strings.HasPrefix(command, "curl "):
					return "prometheus_build_info{version=\"2.55.0\"} 1\n", 0
				case strings.Contains(command, " --version"):
					return "prometheus, version 2.55.0\n", 0
				case strings.Contains(command, "promlifecycle-remote-takeover-probe"):
					launch := fmt.Sprintf("a(sasbttttuii) 1 %q 3 %q %q %q false 0 0 0 0 531 0 0", "/srv/existing/prometheus", "/srv/existing/prometheus", "--config.file="+configPath, "--storage.tsdb.path=/srv/existing/data")
					if scenario == "changed_launch" {
						launch = strings.ReplaceAll(launch, "--storage.tsdb.path=/srv/existing/data", "--storage.tsdb.path=/srv/other/data")
					}
					result := map[string]interface{}{"exec_start": launch, "config_path": configPath, "data_path": "/srv/existing/data", "config_content": encode(remoteConfig), "unit_content": encode(originalUnit), "unit": "existing-prometheus.service"}
					if reject {
						result = map[string]interface{}{"error": "systemd 主进程已变化，请重新扫描"}
					}
					b, _ := json.Marshal(result)
					return string(b), 0
				default:
					return "unexpected remote command", 1
				}
			})
			host := models.Host{ID: 7, Name: "remote", IP: "127.0.0.2", Port: port, Username: "root", Password: "test-only", ConnectionType: "ssh"}
			if err = db.Create(&host).Error; err != nil {
				t.Fatal(err)
			}
			if err = db.Create(&models.Cluster{ID: 1, Name: "single-node"}).Error; err != nil {
				t.Fatal(err)
			}
			if scenario == "database_failure" {
				if err := db.Exec("CREATE TRIGGER reject_adoption BEFORE INSERT ON component_adoptions BEGIN SELECT RAISE(ABORT, 'fixture transaction failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			h := &Handler{db: db, basePath: root}
			router := gin.New()
			router.POST("/hosts/:id/adopt", h.AdoptComponent)
			rows := parseHostDiscovery(host, discovery)
			if len(rows) != 1 {
				t.Fatal("invalid discovery fixture")
			}
			response := connectivityRequest(t, router, "/hosts/7/adopt", map[string]interface{}{"type": "prometheus", "port": healthPort, "ownership": "managed", "fingerprint": rows[0].component.Fingerprint})
			if scenario != "success" {
				expectedCode, reason := http.StatusUnprocessableEntity, "主进程已变化"
				if scenario == "changed_launch" {
					reason = "启动参数"
				}
				if scenario == "health_unreachable" {
					reason = "健康端点"
				}
				if scenario == "database_failure" {
					expectedCode, reason = http.StatusInternalServerError, "保存接管组件失败"
				}
				if response.Code != expectedCode || !strings.Contains(response.Body.String(), reason) {
					t.Fatalf("preflight failure=%d %s", response.Code, response.Body)
				}
				var count int64
				db.Model(&models.Component{}).Count(&count)
				if count != 0 {
					t.Fatal("failed preflight registered component")
				}
				db.Model(&models.ComponentConfig{}).Count(&count)
				if count != 0 {
					t.Fatal("failed transaction retained manual configuration")
				}
				paths, _ := filepath.Glob(filepath.Join(root, "backups", "adoption", "*"))
				if len(paths) != 0 {
					t.Fatal("failed preflight retained snapshot")
				}
				return
			}
			if response.Code != http.StatusCreated {
				t.Fatalf("remote managed adoption=%d %s", response.Code, response.Body)
			}
			var result struct {
				Component models.Component         `json:"component"`
				Adoption  models.ComponentAdoption `json:"adoption"`
			}
			if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Component.DeployType != "binary" || result.Component.ServiceName != "existing-prometheus" || result.Component.ConfigPath != configPath || result.Component.DataPath != "/srv/existing/data" {
				t.Fatalf("lost adopted identity: %+v", result.Component)
			}
			var saved models.Component
			db.First(&saved, result.Component.ID)
			if !strings.Contains(saved.ImportedArgs, "--storage.tsdb.path=/srv/existing/data") {
				t.Fatal("original argv lost")
			}
			var cfg models.ComponentConfig
			if err = db.Where("component_id = ? AND name = ? AND is_active = ?", saved.ID, "prometheus.yml", true).First(&cfg).Error; err != nil || cfg.Content != remoteConfig {
				t.Fatalf("adopted manual configuration not preserved: %v %+v", err, cfg)
			}
			archive, err := os.Open(result.Adoption.SnapshotPath)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			gz, err := gzip.NewReader(archive)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			tr := tar.NewReader(gz)
			entries := map[string]string{}
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(tr)
				entries[hdr.Name] = string(raw)
			}
			if entries["config/existing.yml"] != remoteConfig || entries["systemd/existing-prometheus.service"] != originalUnit {
				t.Fatalf("snapshot omitted remote original files: %v", entries)
			}
			info, _ := archive.Stat()
			if info.Mode().Perm() != 0600 {
				t.Fatalf("snapshot mode=%v", info.Mode())
			}
			sum, _ := checksumFile(result.Adoption.SnapshotPath)
			if sum != result.Adoption.SnapshotChecksum {
				t.Fatal("snapshot checksum mismatch")
			}
			if strings.Contains(response.Body.String(), "31s") {
				t.Fatal("API leaked original config")
			}
			commandsMu.Lock()
			defer commandsMu.Unlock()
			for _, command := range commands {
				if strings.Contains(command, "systemctl stop ") || strings.Contains(command, "systemctl restart ") {
					t.Fatal("adoption changed service state")
				}
			}
		})
	}
}
