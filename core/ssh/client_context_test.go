package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func TestClientContextDeadlineClosesHandshakeAndOSProbe(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "insecure")
	for _, phase := range []string{"handshake", "os-command"} {
		t.Run(phase, func(t *testing.T) {
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
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			closed := make(chan struct{})
			commandStarted := make(chan struct{})
			go func() {
				defer close(closed)
				raw, err := listener.Accept()
				if err != nil {
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(4 * time.Second))
				if phase == "handshake" {
					_, _ = io.Copy(io.Discard, raw)
					return
				}
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
						if request.Type == "exec" {
							_ = request.Reply(true, nil)
							close(commandStarted)
						} else {
							_ = request.Reply(false, nil)
						}
					}
					_ = channel.Close()
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			started := time.Now()
			client, err := NewClientContext(ctx, "127.0.0.1", listener.Addr().(*net.TCPAddr).Port, "root", "secret", "")
			if phase == "handshake" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("handshake error=%v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				if _, err := client.DetectOS(); err == nil {
					t.Fatal("stalled OS command reported success")
				}
				select {
				case <-commandStarted:
				default:
					t.Fatal("OS command never reached SSH peer")
				}
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("probe exceeded total deadline")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("deadline left real SSH transport alive")
			}
		})
	}
}
