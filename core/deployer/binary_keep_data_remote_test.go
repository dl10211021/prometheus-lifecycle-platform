package deployer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/promlifecycle/platform/models"
	gossh "golang.org/x/crypto/ssh"
)

// Execute only canonical-path inspection against local owned fixtures. Every
// service/removal request is recorded but never executed by this SSH peer.
func keepDataReadOnlyPeer(t *testing.T) (models.Host, func() []string) {
	t.Helper()
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "insecure")
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &gossh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var mu sync.Mutex
	var commands []string
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
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
					go func() {
						defer channel.Close()
						for request := range requests {
							if request.Type != "exec" {
								request.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if err := gossh.Unmarshal(request.Payload, &payload); err != nil {
								request.Reply(false, nil)
								return
							}
							request.Reply(true, nil)
							mu.Lock()
							commands = append(commands, payload.Command)
							mu.Unlock()
							status := uint32(0)
							if strings.Contains(payload.Command, "for original in") {
								output, err := exec.Command("sh", "-c", payload.Command).CombinedOutput()
								if err != nil {
									status = 1
								}
								channel.Write(output)
							}
							channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	return models.Host{IP: "127.0.0.2", Port: listener.Addr().(*net.TCPAddr).Port, Username: "fixture", Password: "fixture", ConnectionType: "ssh"}, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), commands...) }
}

func TestRemoveKeepDataSSHResolvesBeforeMutation(t *testing.T) {
	for _, layout := range []string{"symlink_overlap", "loop", "missing_parent", "separated"} {
		t.Run(layout, func(t *testing.T) {
			d, c, root, _ := keepDataFixture(t)
			host, commands := keepDataReadOnlyPeer(t)
			c.Host = host
			actual := filepath.Join(c.InstallPath, "data")
			if layout == "separated" || layout == "missing_parent" {
				actual = filepath.Join(root, "separate-data")
			}
			if err := os.Mkdir(actual, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(actual, "customer.db")
			if err := os.WriteFile(marker, []byte("retained data"), 0600); err != nil {
				t.Fatal(err)
			}
			c.DataPath = filepath.Join(root, "data-alias")
			target := actual
			if layout == "loop" {
				target = c.DataPath
			}
			if err := os.Symlink(target, c.DataPath); err != nil {
				t.Fatal(err)
			}
			if layout == "missing_parent" {
				c.DataPath = filepath.Join(c.DataPath, "absent", "data")
			}
			err := d.RemoveKeepData(context.Background(), c)
			unsafe := layout == "symlink_overlap" || layout == "loop"
			if unsafe && (err == nil || !strings.Contains(err.Error(), "keep_data")) {
				t.Errorf("unsafe remote layout accepted: %v", err)
			}
			if !unsafe && err != nil {
				t.Errorf("safe remote layout rejected: %v", err)
			}
			var resolved, mutated bool
			for _, cmd := range commands() {
				resolved = resolved || strings.Contains(cmd, "for original in")
				mutated = mutated || strings.Contains(cmd, "systemctl stop") || strings.Contains(cmd, "rm -")
			}
			if !resolved {
				t.Error("canonical paths were not inspected over SSH")
			}
			if mutated == unsafe {
				t.Errorf("remote mutation=%v, unsafe=%v", mutated, unsafe)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "retained data" {
				t.Errorf("retained data changed: %q %v", data, err)
			}
		})
	}
}

func TestRemoveKeepDataSSHRejectsIncompleteResolution(t *testing.T) {
	host, commands := binaryMutationPeer(t, "never-fail", 0)
	c := &models.Component{ID: 927362, Type: models.ComponentGrafana, Host: host, DataPath: "/srv/keep-data-fixture"}
	err := NewBinaryDeployer(nil, t.TempDir()).RemoveKeepData(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "keep_data") {
		t.Errorf("incomplete SSH resolution accepted: %v", err)
	}
	for _, cmd := range commands() {
		if strings.Contains(cmd, "systemctl stop") || strings.Contains(cmd, "rm -") {
			t.Errorf("mutation after unresolved SSH response: %s", cmd)
		}
	}
}
