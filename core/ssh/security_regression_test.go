package ssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestUnknownHostKeyModeRejectsAndWhitespaceRetainsStrictVerification(t *testing.T) {
	signer, err := ssh.ParsePrivateKey([]byte(testPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	other, err := ssh.ParsePrivateKey([]byte(testPrivateKey2))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(knownhosts.Line([]string{"127.0.0.1"}, signer.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMLIFECYCLE_SSH_KNOWN_HOSTS", path)
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", " known_hosts \n")
	if err := hostKeyCallback()("127.0.0.1:22", testAddr(), signer.PublicKey()); err != nil {
		t.Fatalf("trimmed strict mode rejected trusted key: %v", err)
	}
	if err := hostKeyCallback()("127.0.0.1:22", testAddr(), other.PublicKey()); err == nil {
		t.Fatal("whitespace disabled strict verification")
	}
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "known_host")
	if err := hostKeyCallback()("127.0.0.1:22", testAddr(), other.PublicKey()); err == nil {
		t.Fatal("invalid mode accepted key")
	}
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", " insecure ")
	if err := hostKeyCallback()("127.0.0.1:22", testAddr(), other.PublicKey()); err != nil {
		t.Fatalf("explicit insecure mode rejected: %v", err)
	}
}

func TestCancelledCommandDoesNotOpenSSHSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var client Client
	_, _, err := client.RunCommandContext(ctx, "touch should-not-run")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled command error: %v", err)
	}
}
