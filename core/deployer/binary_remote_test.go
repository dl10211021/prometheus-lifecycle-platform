package deployer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	gossh "golang.org/x/crypto/ssh"
)

// The peer accepts real SSH sessions but never executes destructive commands.
// Its recorded protocol requests expose whether recovery/cleanup was dispatched.
func binaryMutationPeer(t *testing.T, fail string, exit uint32) (models.Host, func() []string) {
	t.Helper()
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "insecure")
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := gossh.NewSignerFromKey(private)
	config := &gossh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commands []string
	t.Cleanup(func() { listener.Close() })
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
							gossh.Unmarshal(request.Payload, &payload)
							request.Reply(true, nil)
							mu.Lock()
							commands = append(commands, payload.Command)
							mu.Unlock()
							if strings.Contains(payload.Command, "cat >") {
								io.Copy(io.Discard, channel)
							}
							status := uint32(0)
							if strings.Contains(payload.Command, fail) {
								status = exit
							}
							channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	return models.Host{IP: "127.0.0.2", Port: listener.Addr().(*net.TCPAddr).Port, Username: "test", Password: "fixture", ConnectionType: "ssh"}, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), commands...) }
}

func TestBinaryRemoteUnknownMutationSuppressesRecoveryAndCleanup(t *testing.T) {
	for _, phase := range []string{"tree-swap", "restart", "remove"} {
		t.Run(phase, func(t *testing.T) {
			fail := "mv -T"
			if phase == "restart" {
				fail = "systemctl restart"
			}
			if phase == "remove" {
				fail = "systemctl stop"
			}
			host, commands := binaryMutationPeer(t, fail, 124)
			base := t.TempDir()
			cache := filepath.Join(base, "bin", "grafana-v2")
			os.MkdirAll(cache, 0755)
			os.WriteFile(filepath.Join(cache, "fixture"), []byte("new release"), 0644)
			component := &models.Component{ID: 42, Type: models.ComponentGrafana, Host: host, ServiceName: "fixture-grafana"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			d := NewBinaryDeployer(nil, base)
			var err error
			if phase == "remove" {
				err = d.Remove(ctx, component)
			} else {
				err = d.Upgrade(ctx, component, "v2")
			}
			if !errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
				t.Fatalf("lost uncertain mutation: %v", err)
			}
			seenFailure := false
			for _, cmd := range commands() {
				if strings.Contains(cmd, fail) {
					seenFailure = true
					continue
				}
				if seenFailure && (strings.Contains(cmd, "rm -rf") || strings.Contains(cmd, "systemctl stop") || strings.Contains(cmd, "mv ")) {
					t.Fatalf("unsafe command after unknown mutation: %s", cmd)
				}
			}
			if !seenFailure {
				t.Fatal("fixture did not exercise mutation")
			}
		})
	}
}

func TestBinaryRemoteConfirmedRestartFailureRestoresFiles(t *testing.T) {
	host, commands := binaryMutationPeer(t, "systemctl restart", 1)
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "bin"), 0755)
	os.WriteFile(filepath.Join(base, "bin", "node_exporter-v2"), []byte("new binary"), 0755)
	component := &models.Component{ID: 43, Type: models.ComponentNodeExporter, Host: host, ServiceName: "fixture-node"}
	err := NewBinaryDeployer(nil, base).Upgrade(context.Background(), component, "v2")
	if err == nil || errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) || errors.Is(err, ErrBinaryRecoveryUnconfirmed) {
		t.Fatalf("confirmed failure not safely recoverable: %v", err)
	}
	var stopped, restored bool
	for _, cmd := range commands() {
		stopped = stopped || strings.Contains(cmd, "systemctl stop")
		restored = restored || (strings.Contains(cmd, "mv -f") && strings.Contains(cmd, ".bak"))
	}
	if !stopped || !restored {
		t.Fatalf("confirmed failure did not stop/restore files: %v", commands())
	}
}
