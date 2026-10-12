package services

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A separate process is essential: the unrelated listener must not share the
// selected PID's fd table, as two httptest servers in this process would.
func TestPrometheusRestoreExecutionSocketHelper(t *testing.T) {
	address := os.Getenv("PLC_EXECUTION_SOCKET_ADDRESS")
	if address == "" {
		return
	}
	network := os.Getenv("PLC_EXECUTION_SOCKET_NETWORK")
	if delay := os.Getenv("PLC_EXECUTION_SOCKET_DELAY"); delay != "" {
		fmt.Println(address)
		d, _ := time.ParseDuration(delay)
		time.Sleep(d)
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		panic(err)
	}
	if os.Getenv("PLC_EXECUTION_SOCKET_DELAY") == "" {
		fmt.Println(listener.Addr().String())
	}
	statusPath := os.Getenv("PLC_EXECUTION_SOCKET_STATUS")
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, _ := os.ReadFile(statusPath)
		if string(status) != "ready" {
			w.WriteHeader(503)
			return
		}
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		host := os.Getenv("PLC_EXECUTION_SOCKET_HOST")
		if host == "" {
			host = "localhost"
		}
		if r.Host != host+":"+port || r.URL.RequestURI() != "/saved/-/ready?tenant=x" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, "owned-ready")
	}))
}

func executionSocketProcess(t *testing.T, network, address, status, delay string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPrometheusRestoreExecutionSocketHelper$")
	cmd.Env = append(os.Environ(), "PLC_EXECUTION_SOCKET_NETWORK="+network, "PLC_EXECUTION_SOCKET_ADDRESS="+address, "PLC_EXECUTION_SOCKET_STATUS="+status, "PLC_EXECUTION_SOCKET_DELAY="+delay)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal("socket helper failed", err)
	}
	return strconv.Itoa(cmd.Process.Pid), strings.TrimSpace(line)
}

func TestPrometheusRestoreExecutionReadinessPinned(t *testing.T) {
	status := filepath.Join(t.TempDir(), "owned-status")
	if err := os.WriteFile(status, []byte("unready"), 0600); err != nil {
		t.Fatal(err)
	}
	pid, address := executionSocketProcess(t, "tcp4", "127.0.0.1:0", status, "")
	_, port, _ := net.SplitHostPort(address)
	other := filepath.Join(t.TempDir(), "other-status")
	if err := os.WriteFile(other, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = executionSocketProcess(t, "tcp6", "[::1]:"+port, other, "")
	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyRequests.Add(1); fmt.Fprint(w, "proxy-ready") }))
	defer proxy.Close()
	for _, key := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(key, proxy.URL)
	}
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")
	ready := "http://localhost:" + port + "/saved/-/ready?tenant=x"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err := waitRestoreExecutionReadiness(ctx, localBackupRunner{}, ready, pid)
	cancel()
	if err == nil {
		t.Fatal("unrelated IPv6/proxy readiness accepted for unready selected IPv4 process")
	}
	if proxyRequests.Load() != 0 {
		t.Fatal("readiness used an inherited proxy")
	}
	if err = os.WriteFile(status, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = waitRestoreExecutionReadiness(ctx, localBackupRunner{}, ready, pid); err != nil {
		t.Fatal("owned readiness or saved route/Host not preserved", err)
	}
	if proxyRequests.Load() != 0 {
		t.Fatal("healthy readiness used an inherited proxy")
	}
}

func TestPrometheusRestoreExecutionReadinessWaitsForOwnedListener(t *testing.T) {
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	reservation.Close()
	pid, _ := executionSocketProcess(t, "tcp4", address, status, "200ms")
	_, port, _ := net.SplitHostPort(address)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitRestoreExecutionReadiness(ctx, localBackupRunner{}, "http://localhost:"+port+"/saved/-/ready?tenant=x", pid); err != nil {
		t.Fatal("selected process listener startup was not retried", err)
	}
}

func TestPrometheusRestoreExecutionIPv6OnlyWildcardDoesNotProveIPv4(t *testing.T) {
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	pid, address := executionSocketProcess(t, "tcp6", "[::]:0", status, "")
	_, port, _ := net.SplitHostPort(address)
	if err := verifyExecutionListener(context.Background(), localBackupRunner{}, "http://127.0.0.1:"+port+"/-/ready", pid); err == nil {
		t.Fatal("IPv6-only wildcard accepted as ownership proof for IPv4 connection")
	}
}

func TestPrometheusRestoreExecutionReadinessOwnedIPv6LocalEndpoint(t *testing.T) {
	for _, fixture := range []struct{ name, network, address string }{{"generated-dual-stack-wildcard", "tcp", "[::]:0"}, {"owned-ipv6-loopback", "tcp6", "[::1]:0"}} {
		t.Run(fixture.name, func(t *testing.T) {
			status := filepath.Join(t.TempDir(), "status")
			if err := os.WriteFile(status, []byte("ready"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PLC_EXECUTION_SOCKET_HOST", "127.0.0.1")
			pid, address := executionSocketProcess(t, fixture.network, fixture.address, status, "")
			_, port, _ := net.SplitHostPort(address)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := waitRestoreExecutionReadiness(ctx, localBackupRunner{}, "http://127.0.0.1:"+port+"/saved/-/ready?tenant=x", pid); err != nil {
				t.Fatal("owned IPv6 local endpoint did not preserve original IPv4 URL identity", err)
			}
		})
	}
}

type executionHTTPBoundaryRecorder struct{ requests int }

func (r *executionHTTPBoundaryRecorder) Run(ctx context.Context, command string) (string, error) {
	if strings.HasPrefix(command, "curl -fsS") {
		r.requests++
		return "", nil
	}
	return (localBackupRunner{}).Run(ctx, command)
}

func TestPrometheusRestoreExecutionReadinessRejectsNonlocalWildcardDestination(t *testing.T) {
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	pid, address := executionSocketProcess(t, "tcp4", "0.0.0.0:0", status, "")
	_, port, _ := net.SplitHostPort(address)
	// A real owned wildcard listener must never authorize HTTP to another host.
	// Intercept that boundary so the red regression cannot send external traffic.
	target := "192.0.2.1"
	route, _ := exec.Command("ip", "-4", "route", "get", target).Output()
	if strings.HasPrefix(string(route), "local "+target+" ") {
		t.Skip("documentation address is local on this test host")
	}
	r := &executionHTTPBoundaryRecorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := waitRestoreExecutionReadiness(ctx, r, "http://"+net.JoinHostPort(target, port)+"/saved/-/ready?tenant=x", pid)
	if err == nil || r.requests != 0 {
		t.Fatalf("nonlocal wildcard destination reached HTTP: requests=%d error=%v", r.requests, err)
	}
}

func TestPrometheusRestoreExecutionReadinessAcceptsKernelLocalIPv4ThroughOwnedDualStackWildcard(t *testing.T) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	host := ""
	for _, address := range addresses {
		subnet, ok := address.(*net.IPNet)
		if ok && subnet.IP.To4() != nil && !subnet.IP.IsLoopback() {
			host = subnet.IP.String()
			break
		}
	}
	if host == "" {
		t.Skip("no non-loopback IPv4 interface")
	}
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLC_EXECUTION_SOCKET_HOST", host)
	pid, address := executionSocketProcess(t, "tcp", "[::]:0", status, "")
	_, port, _ := net.SplitHostPort(address)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitRestoreExecutionReadiness(ctx, localBackupRunner{}, "http://"+net.JoinHostPort(host, port)+"/saved/-/ready?tenant=x", pid); err != nil {
		t.Fatal("owned dual-stack wildcard did not prove the original local IPv4 endpoint", err)
	}
}

func TestPrometheusRestoreExecutionReadinessAcceptsKernelLocalWildcardDestination(t *testing.T) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	host := ""
	for _, address := range addresses {
		subnet, ok := address.(*net.IPNet)
		if ok && subnet.IP.To4() != nil && !subnet.IP.IsLoopback() {
			host = subnet.IP.String()
			break
		}
	}
	if host == "" {
		t.Skip("no non-loopback IPv4 interface")
	}
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLC_EXECUTION_SOCKET_HOST", host)
	pid, address := executionSocketProcess(t, "tcp4", "0.0.0.0:0", status, "")
	_, port, _ := net.SplitHostPort(address)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitRestoreExecutionReadiness(ctx, localBackupRunner{}, "http://"+net.JoinHostPort(host, port)+"/saved/-/ready?tenant=x", pid); err != nil {
		t.Fatal("kernel-local wildcard destination rejected", err)
	}
}

func executionNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		subnet, ok := address.(*net.IPNet)
		if ok && subnet.IP.To4() != nil && !subnet.IP.IsLoopback() {
			return subnet.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 interface")
	return ""
}

type executionPathRunner struct{ dir string }

func (r executionPathRunner) Run(ctx context.Context, command string) (string, error) {
	return (localBackupRunner{}).Run(ctx, "PATH="+strconv.Quote(r.dir)+":$PATH; export PATH; "+command)
}

func TestPrometheusRestoreExecutionRejectsUntrustedDualStackSocketEvidence(t *testing.T) {
	host := executionNonLoopbackIPv4(t)
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLC_EXECUTION_SOCKET_HOST", host)
	pid, address := executionSocketProcess(t, "tcp", "[::]:0", status, "")
	_, port, _ := net.SplitHostPort(address)
	ssPath, err := exec.LookPath("ss")
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		name, script string
	}{
		{"missing-v6only", fmt.Sprintf("#!/bin/sh\n%s \"$@\" | sed 's/v6only:0//g'\n", strconv.Quote(ssPath))},
		{"malformed-v6only", fmt.Sprintf("#!/bin/sh\n%s \"$@\" | sed 's/v6only:0/v6only:bogus/g'\n", strconv.Quote(ssPath))},
		{"foreign-inode", fmt.Sprintf("#!/bin/sh\n%s \"$@\" | sed -E 's/ino:[0-9]+/ino:1/g'\n", strconv.Quote(ssPath))},
		{"conflicting-inode", fmt.Sprintf("#!/bin/sh\n%s \"$@\" | sed -E 's/ v6only:/ ino:1 v6only:/'\n", strconv.Quote(ssPath))},
		{"malformed-inode", fmt.Sprintf("#!/bin/sh\n%s \"$@\" | sed -E 's/ v6only:/ ino:bogus v6only:/'\n", strconv.Quote(ssPath))},
		{"ambiguous-lines", fmt.Sprintf("#!/bin/sh\n%s \"$@\"\n%s \"$@\"\n", strconv.Quote(ssPath), strconv.Quote(ssPath))},
		{"tool-error", "#!/bin/sh\nexit 7\n"},
		{"oversized", fmt.Sprintf("#!/bin/sh\n%s \"$@\"\npython3 -c 'print(\"x\"*5000)'\n", strconv.Quote(ssPath))},
		{"oversized-newlines", fmt.Sprintf("#!/bin/sh\n%s \"$@\"\npython3 -c 'print(\"\\n\"*5000,end=\"\")'\n", strconv.Quote(ssPath))},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ss"), []byte(fixture.script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := verifyExecutionListener(context.Background(), executionPathRunner{dir: dir},
				"http://"+net.JoinHostPort(host, port)+"/-/ready", pid); err == nil {
				t.Fatal("untrusted dual-stack socket evidence was accepted")
			}
		})
	}
}

func TestPrometheusRestoreExecutionRejectsDualStackOwnershipMismatch(t *testing.T) {
	host := executionNonLoopbackIPv4(t)
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("ipv6-only", func(t *testing.T) {
		pid, address := executionSocketProcess(t, "tcp6", "[::]:0", status, "")
		_, port, _ := net.SplitHostPort(address)
		if err := verifyExecutionListener(context.Background(), localBackupRunner{},
			"http://"+net.JoinHostPort(host, port)+"/-/ready", pid); err == nil {
			t.Fatal("IPv6-only wildcard accepted as IPv4 ownership proof")
		}
	})
	t.Run("foreign-owner", func(t *testing.T) {
		_, address := executionSocketProcess(t, "tcp", "[::]:0", status, "")
		_, port, _ := net.SplitHostPort(address)
		if err := verifyExecutionListener(context.Background(), localBackupRunner{},
			"http://"+net.JoinHostPort(host, port)+"/-/ready", strconv.Itoa(os.Getpid())); err == nil {
			t.Fatal("foreign dual-stack socket accepted as selected PID ownership proof")
		}
	})
	t.Run("competing-native-ipv4", func(t *testing.T) {
		pid, address := executionSocketProcess(t, "tcp6", "[::]:0", status, "")
		_, port, _ := net.SplitHostPort(address)
		_, _ = executionSocketProcess(t, "tcp4", "0.0.0.0:"+port, status, "")
		dir := t.TempDir()
		marker := filepath.Join(dir, "ss-called")
		fakeSS := "#!/bin/sh\nprintf called >" + strconv.Quote(marker) + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, "ss"), []byte(fakeSS), 0700); err != nil {
			t.Fatal(err)
		}
		if err := verifyExecutionListener(context.Background(), executionPathRunner{dir: dir},
			"http://"+net.JoinHostPort(host, port)+"/-/ready", pid); err == nil {
			t.Fatal("dual-stack fallback bypassed a competing native IPv4 listener")
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("competing native IPv4 listener reached the dual-stack option probe")
		}
	})
}

type executionReadinessCommandRecorder struct{ commands []string }

func (r *executionReadinessCommandRecorder) Run(ctx context.Context, command string) (string, error) {
	r.commands = append(r.commands, command)
	return (localBackupRunner{}).Run(ctx, command)
}

func TestPrometheusRestoreExecutionDualStackHTTPFailureStaysOnRecordedIPv4(t *testing.T) {
	host := executionNonLoopbackIPv4(t)
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("unready"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLC_EXECUTION_SOCKET_HOST", host)
	pid, address := executionSocketProcess(t, "tcp", "[::]:0", status, "")
	_, port, _ := net.SplitHostPort(address)
	runner := &executionReadinessCommandRecorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := waitRestoreExecutionReadiness(ctx, runner,
		"http://"+net.JoinHostPort(host, port)+"/saved/-/ready?tenant=x", pid); err == nil {
		t.Fatal("unready recorded IPv4 endpoint was accepted")
	}
	curlCount := 0
	for _, command := range runner.commands {
		if !strings.HasPrefix(command, "curl -fsS") {
			continue
		}
		curlCount++
		if !strings.Contains(command, host) || strings.Contains(command, "127.0.0.1") || strings.Contains(command, "[::1]") {
			t.Fatalf("HTTP left the recorded IPv4 endpoint: %s", command)
		}
	}
	if curlCount == 0 {
		t.Fatal("owned dual-stack listener never reached the recorded IPv4 HTTP check")
	}
}
