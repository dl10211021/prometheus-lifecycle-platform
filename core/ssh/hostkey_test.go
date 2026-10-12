package ssh

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
}

func TestHostKeyCallbackModes(t *testing.T) {
	signer, err := ssh.ParsePrivateKey([]byte(testPrivateKey))
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	key := signer.PublicKey()
	signer2, err := ssh.ParsePrivateKey([]byte(testPrivateKey2))
	if err != nil {
		t.Fatalf("parse second test key: %v", err)
	}
	key2 := signer2.PublicKey()
	resetAcceptedKeys := func() {
		acceptedKeys.Lock()
		acceptedKeys.m = map[string]ssh.PublicKey{}
		acceptedKeys.Unlock()
	}

	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	t.Setenv("PROMLIFECYCLE_SSH_KNOWN_HOSTS", kh)
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "")
	resetAcceptedKeys()
	if err := hostKeyCallback()("127.0.0.1:22", testAddr(), key); err != nil {
		t.Fatalf("default accept_new mode rejected first use: %v", err)
	}
	info, err := os.Stat(kh)
	if err != nil {
		t.Fatalf("default accept_new mode did not create known_hosts: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("known_hosts permissions = %04o, want 0600", info.Mode().Perm())
	}
	if data, err := os.ReadFile(kh); err != nil || len(data) == 0 {
		t.Fatalf("default accept_new mode did not persist host key: bytes=%d err=%v", len(data), err)
	}

	// Simulate a process restart: the persisted key must still accept the same
	// host and reject a changed key.
	resetAcceptedKeys()
	cb := hostKeyCallback()
	if err := cb("127.0.0.1:22", testAddr(), key); err != nil {
		t.Fatalf("persisted host key was not accepted: %v", err)
	}
	if err := cb("127.0.0.1:22", testAddr(), key2); err == nil {
		t.Fatal("accept_new must reject a changed persisted key")
	}

	// Strict mode must fail closed when its configured file is unavailable.
	missing := filepath.Join(t.TempDir(), "missing", "known_hosts")
	t.Setenv("PROMLIFECYCLE_SSH_KNOWN_HOSTS", missing)
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "known_hosts")
	if err := hostKeyCallback()("strict-host:22", testAddr(), key); err == nil {
		t.Fatal("known_hosts mode accepted a host without a usable trust file")
	}

	// Strict mode with a matching entry accepts it and rejects a mismatch.
	t.Setenv("PROMLIFECYCLE_SSH_KNOWN_HOSTS", kh)
	cb = hostKeyCallback()
	if err := cb("127.0.0.1:22", testAddr(), key); err != nil {
		t.Fatalf("known_hosts mode should accept matching key: %v", err)
	}
	if err := cb("127.0.0.1:22", testAddr(), key2); err == nil {
		t.Fatal("known_hosts mode must reject mismatched key")
	}

	// accept_new still trusts and persists a genuinely unknown second host.
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "accept_new")
	resetAcceptedKeys()
	cb = hostKeyCallback()
	if err := cb("newhost:22", testAddr(), key2); err != nil {
		t.Fatalf("accept_new should trust an unknown host: %v", err)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

var testPrivateKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDOdWFwhGu+oMjpEfBEVKrx03ZfyxmNH507xP/f9OJAAwAAAJib6/A5m+vw
OQAAAAtzc2gtZWQyNTUxOQAAACDOdWFwhGu+oMjpEfBEVKrx03ZfyxmNH507xP/f9OJAAw
AAAECh0HAJOE1v0B4HlghMe+fROWcfL3l35tU87kkyI4x/f851YXCEa76gyOkR8ERUqvHT
dl/LGY0fnTvE/9/04kADAAAAFHJvb3RAZGVzaHVvLTAwMDEwODY0AQ==
-----END OPENSSH PRIVATE KEY-----`

var testPrivateKey2 = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACAUAG8+IYfhF9Hh8l1GRzaRL1dd49X3NtSC6xSOV4zaqgAAAJhmLmwFZi5s
BQAAAAtzc2gtZWQyNTUxOQAAACAUAG8+IYfhF9Hh8l1GRzaRL1dd49X3NtSC6xSOV4zaqg
AAAEAodEr8SeD59bTV+psLJYxSGE3xyynOVYYzRnU52q5C+RQAbz4hh+EX0eHyXUZHNpEv
V13j1fc21ILrFI5XjNqqAAAAFHJvb3RAZGVzaHVvLTAwMDEwODY0AQ==
-----END OPENSSH PRIVATE KEY-----`

func TestPatternMatchesPlainAndHashed(t *testing.T) {
	// Plain entry: case-insensitive, port-normalized target
	if !patternMatches("myhost", "myhost:22") {
		t.Fatal("plain pattern must match normalized host")
	}
	if patternMatches("myhost:2222", "myhost:22") {
		t.Fatal("plain pattern with different port must not match")
	}
	if patternMatches("otherhost", "myhost:22") {
		t.Fatal("different host must not match")
	}

	// Hashed entry: recompute HMAC-SHA1 with the entry's salt
	hashed := knownhosts.HashHostname("myhost:22")
	if !patternMatches(hashed, "myhost:22") {
		t.Fatal("hashed pattern must match its own host")
	}
	if patternMatches(hashed, "myhost:2222") {
		t.Fatal("hashed pattern must not match a different port")
	}
	// Malformed hash patterns never match
	for _, bad := range []string{"|1|onlysalt", "|2|aGVsbG8=|aGVsbG8=", "|1|!!!|aGVsbG8="} {
		if patternMatches(bad, "myhost:22") {
			t.Fatalf("malformed pattern %q must not match", bad)
		}
	}
}

func TestHostKeyAlgorithmsForPrefersRecorded(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	// ED25519 recorded for the target host (hashed, like OpenSSH writes by default)
	line := knownhosts.HashHostname(knownhosts.Normalize("10.0.0.9:22")) + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA==\n"
	if err := os.WriteFile(kh, []byte(line), 0600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	os.Setenv("PROMLIFECYCLE_SSH_KNOWN_HOSTS", kh)
	defer os.Unsetenv("PROMLIFECYCLE_SSH_KNOWN_HOSTS")

	algos := hostKeyAlgorithmsFor("10.0.0.9", 22)
	if len(algos) == 0 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("recorded ED25519 must be offered first, got %v", algos)
	}
	// All defaults still present, no duplicates
	seen := map[string]int{}
	for _, a := range algos {
		seen[a]++
	}
	for _, want := range []string{ssh.KeyAlgoECDSA256, ssh.KeyAlgoED25519, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA} {
		if seen[want] != 1 {
			t.Fatalf("expected %s exactly once, got %d (list=%v)", want, seen[want], algos)
		}
	}

	// Unrecorded host keeps the default order
	algos = hostKeyAlgorithmsFor("10.9.9.9", 22)
	if algos[0] != ssh.KeyAlgoECDSA256 {
		t.Fatalf("unknown host must keep default order, got %v", algos)
	}
}

func TestHostKeyAlgoNames(t *testing.T) {
	if got := hostKeyAlgoNames("ssh-ed25519"); len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("ed25519 mapping wrong: %v", got)
	}
	if got := hostKeyAlgoNames("ssh-rsa"); len(got) != 2 {
		t.Fatalf("rsa must authorize both SHA-2 schemes: %v", got)
	}
	if got := hostKeyAlgoNames("ssh-dss"); got != nil {
		t.Fatalf("unsupported types must map to nil: %v", got)
	}
}
