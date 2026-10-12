package ssh

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
)

type Client struct {
	*ssh.Client
	Host       string
	Port       int
	stopCancel func() bool
}

// acceptedKeys remembers host keys accepted via trust-on-first-use
// (PROMLIFECYCLE_SSH_HOST_KEY_MODE=accept_new) for the lifetime of the process.
var acceptedKeys = struct {
	sync.Mutex
	m map[string]ssh.PublicKey
}{m: map[string]ssh.PublicKey{}}

func NewClient(host string, port int, username string, password string, keyPath string) (*Client, error) {
	return NewClientContext(context.Background(), host, port, username, password, keyPath)
}

// NewClientContext keeps the context attached to the transport for its entire
// lifetime, bounding dialing, SSH negotiation and subsequent OS probes together.
func NewClientContext(ctx context.Context, host string, port int, username string, password string, keyPath string) (*Client, error) {
	const maxAttempts = 8

	// Callers pass Host struct fields whose values depend on GORM hook
	// timing: BeforeSave encrypts credentials IN PLACE on any save (including
	// unrelated status updates on the same struct), AfterFind decrypts on
	// load. Decrypt here so NewClient accepts either form — this is the
	// single choke point every SSH operation flows through.
	if pw, err := credentials.Decrypt(password); err == nil {
		password = pw
	}
	if pk, err := credentials.Decrypt(keyPath); err == nil {
		keyPath = pk
	}

	var authMethods []ssh.AuthMethod

	if password != "" {
		authMethods = append(authMethods, ssh.Password(password))
	}

	if keyPath != "" {
		if key, err := ssh.ParsePrivateKey([]byte(keyPath)); err == nil {
			authMethods = append(authMethods, ssh.PublicKeys(key))
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("未提供任何认证凭据：请填写密码或粘贴 SSH 私钥（二者至少其一）")
	}

	config := &ssh.ClientConfig{
		User:              username,
		Auth:              authMethods,
		HostKeyCallback:   hostKeyCallback(),
		HostKeyAlgorithms: hostKeyAlgorithmsFor(host, port),
		Timeout:           15 * time.Second,
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var conn *ssh.Client
	var stopCancel func() bool
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var raw net.Conn
		raw, err = (&net.Dialer{Timeout: config.Timeout}).DialContext(ctx, "tcp", addr)
		if err == nil {
			stopCancel = context.AfterFunc(ctx, func() { _ = raw.Close() })
			deadline := time.Now().Add(config.Timeout)
			if total, ok := ctx.Deadline(); ok && total.Before(deadline) {
				deadline = total
			}
			_ = raw.SetDeadline(deadline)
			sshConn, channels, requests, handshakeErr := ssh.NewClientConn(raw, addr, config)
			err = handshakeErr
			if err == nil {
				deadline = time.Time{}
				if total, ok := ctx.Deadline(); ok {
					deadline = total
				}
				_ = raw.SetDeadline(deadline)
				conn = ssh.NewClient(sshConn, channels, requests)
				break
			}
			stopCancel()
			_ = raw.Close()
		}
		if attempt == maxAttempts {
			break
		}
		// Transient failures (connection reset / sshd MaxStartups) are common on
		// busy hosts; retry with a short backoff before giving up.
		log.Printf("[SSH] dial %s failed (attempt %d/%d): %v, retrying", addr, attempt, maxAttempts, err)
		backoff := []time.Duration{1500 * time.Millisecond, 3 * time.Second, 5 * time.Second, 8 * time.Second, 12 * time.Second, 20 * time.Second, 30 * time.Second}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff[attempt-1]):
		}
	}
	if ctx.Err() != nil {
		if conn != nil {
			stopCancel()
			_ = conn.Close()
		}
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}

	return &Client{Client: conn, Host: host, Port: port, stopCancel: stopCancel}, nil
}

func (c *Client) Close() error {
	if c.stopCancel != nil {
		c.stopCancel()
	}
	return c.Client.Close()
}

// hostKeyCallback returns a callback based on PROMLIFECYCLE_SSH_HOST_KEY_MODE:
//   - accept_new (default): trust-on-first-use. Unknown hosts are accepted and
//     their key is persisted to the platform known_hosts file; a host whose
//     key CHANGED is rejected with an actionable error. This keeps new hosts
//     working out of the box while closing the MITM window after first use.
//   - known_hosts: strict verification against ~/.ssh/known_hosts; hosts not
//     listed are rejected, mismatches return an actionable error message.
//   - insecure: no host key verification (legacy behavior; only for
//     air-gapped labs or hosts with rotating keys - do not use in production).
func hostKeyCallback() ssh.HostKeyCallback {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE")))
	if mode == "" {
		mode = "accept_new"
	}

	strict, strictPath := loadKnownHostsCallback()
	rejectUnavailable := func(mode string, cause error) ssh.HostKeyCallback {
		return func(_ string, _ net.Addr, _ ssh.PublicKey) error {
			return fmt.Errorf("SSH 主机密钥校验不可用（模式=%s，文件=%s）：%w", mode, strictPath, cause)
		}
	}

	switch mode {
	case "known_hosts":
		if strict != nil {
			return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
				if err := strict(hostname, remote, key); err != nil {
					return fmt.Errorf("SSH 主机密钥校验失败（请确认 known_hosts 中的密钥是否已过期；如需信任新密钥请更新 %s，或设置 PROMLIFECYCLE_SSH_HOST_KEY_MODE=accept_new 后重启）: %w", strictPath, err)
				}
				return nil
			}
		}
		err := fmt.Errorf("known_hosts 文件不存在、为空或格式无效")
		log.Printf("[SSH] ERROR: PROMLIFECYCLE_SSH_HOST_KEY_MODE=known_hosts requires a usable file at %s", strictPath)
		return rejectUnavailable(mode, err)
	case "accept_new":
		if strict == nil {
			var err error
			strict, err = initializeKnownHosts(strictPath)
			if err != nil {
				log.Printf("[SSH] ERROR: could not initialize accept_new known_hosts file %s: %v", strictPath, err)
				return rejectUnavailable(mode, err)
			}
		}
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			acceptedKeys.Lock()
			defer acceptedKeys.Unlock()
			if prev, seen := acceptedKeys.m[hostname]; seen &&
				prev.Type() == key.Type() && string(prev.Marshal()) == string(key.Marshal()) {
				return nil
			}
			err := strict(hostname, remote, key)
			if err == nil {
				acceptedKeys.m[hostname] = key
				return nil
			}
			if isUnknownHostKey(err) {
				// Trust on first use only when the key can be durably persisted;
				// otherwise a restart would reopen the MITM window.
				if persistErr := persistAcceptedKey(strictPath, hostname, key); persistErr != nil {
					return fmt.Errorf("无法持久化 SSH 主机密钥到 %s，已拒绝连接: %w", strictPath, persistErr)
				}
				acceptedKeys.m[hostname] = key
				return nil
			}
			return fmt.Errorf("SSH 主机密钥校验失败（主机密钥与 known_hosts 不一致，可能服务器已重装或遭遇中间人攻击；如确认安全请更新 %s 后重试）: %w", strictPath, err)
		}
	case "insecure":
		log.Printf("[SSH] WARNING: host key verification disabled (PROMLIFECYCLE_SSH_HOST_KEY_MODE=%s)", mode)
		return ssh.InsecureIgnoreHostKey()
	default:
		return rejectUnavailable(mode, fmt.Errorf("invalid PROMLIFECYCLE_SSH_HOST_KEY_MODE: %q", mode))
	}
}

func initializeKnownHosts(path string) (ssh.HostKeyCallback, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("known_hosts 路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("创建 known_hosts 目录: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建 known_hosts 文件: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("关闭 known_hosts 文件: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("限制 known_hosts 文件权限: %w", err)
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("加载 known_hosts 文件: %w", err)
	}
	return callback, nil
}

// persistAcceptedKey appends a trusted host key line to the known_hosts file
// (created 0600), so accept_new trust survives restarts.
func persistAcceptedKey(path, hostname string, key ssh.PublicKey) error {
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// loadKnownHostsCallback loads a strict callback from the platform known_hosts
// file (env PROMLIFECYCLE_SSH_KNOWN_HOSTS, else ~/.ssh/known_hosts), or
// (nil, path) when no usable file exists.
func loadKnownHostsCallback() (ssh.HostKeyCallback, string) {
	path := os.Getenv("PROMLIFECYCLE_SSH_KNOWN_HOSTS")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".ssh", "known_hosts")
		}
	}
	if path == "" {
		return nil, ""
	}
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		if cb, err := knownhosts.New(path); err == nil {
			return cb, path
		}
	}
	return nil, path
}

// hostKeyAlgorithmsFor returns the host key algorithms to offer during key
// exchange, preferring those already recorded in the platform known_hosts
// file for this host. Go's default order starts with ECDSA, so a server that
// offers several host keys (RSA/ECDSA/ED25519) while known_hosts records only
// the ED25519 one fails verification with "knownhosts: key mismatch" — the
// negotiated ECDSA key simply is not the recorded one. OpenSSH avoids this by
// preferring algorithms already trusted for the host; we do the same.
// Unknown hosts fall back to the default order.
func hostKeyAlgorithmsFor(host string, port int) []string {
	defaults := []string{
		ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoED25519, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA,
	}
	path := knownHostsPath()
	if path == "" {
		return defaults
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return defaults
	}
	target := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
	pref := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "!") {
			continue
		}
		for _, pattern := range strings.Split(fields[0], ",") {
			if patternMatches(pattern, target) {
				for _, algo := range hostKeyAlgoNames(fields[1]) {
					pref[algo] = true
				}
			}
		}
	}
	if len(pref) == 0 {
		return defaults
	}
	ordered := make([]string, 0, len(pref)+len(defaults))
	for _, algo := range defaults {
		if pref[algo] {
			ordered = append(ordered, algo)
		}
	}
	for _, algo := range defaults {
		if !pref[algo] {
			ordered = append(ordered, algo)
		}
	}
	return ordered
}

// knownHostsPath resolves the platform known_hosts file location
// (env PROMLIFECYCLE_SSH_KNOWN_HOSTS, else ~/.ssh/known_hosts), or "".
func knownHostsPath() string {
	if path := os.Getenv("PROMLIFECYCLE_SSH_KNOWN_HOSTS"); path != "" {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".ssh", "known_hosts")
	}
	return ""
}

// patternMatches reports whether a known_hosts host pattern matches the
// normalized target ("host" for port 22, "[host]:port" otherwise). Hashed
// entries (|1|salt|hash) are verified by recomputing HMAC-SHA1 over the
// target with the entry's own salt; plain entries are compared
// case-insensitively, with an unqualified pattern matching port 22 only.
func patternMatches(pattern, target string) bool {
	if strings.HasPrefix(pattern, "|1|") {
		parts := strings.Split(pattern, "|") // ["", "1", salt, hash]
		if len(parts) != 4 {
			return false
		}
		salt, err1 := base64.StdEncoding.DecodeString(parts[2])
		want, err2 := base64.StdEncoding.DecodeString(parts[3])
		if err1 != nil || err2 != nil {
			return false
		}
		mac := hmac.New(sha1.New, salt)
		mac.Write([]byte(target))
		return hmac.Equal(mac.Sum(nil), want)
	}
	pHost, pPort := pattern, "22"
	if h, p, err := net.SplitHostPort(pattern); err == nil {
		pHost, pPort = h, p
	}
	tHost, tPort := target, "22"
	if h, p, err := net.SplitHostPort(target); err == nil {
		tHost, tPort = h, p
	}
	return strings.EqualFold(pHost, tHost) && pPort == tPort
}

// hostKeyAlgoNames maps a known_hosts key type to the client-side host key
// algorithm names it authorizes. Certificate variants collapse to their base
// algorithm; an RSA key authorizes both SHA-2 signature schemes.
func hostKeyAlgoNames(keyType string) []string {
	switch keyType {
	case "ssh-ed25519", ssh.CertAlgoED25519v01:
		return []string{ssh.KeyAlgoED25519}
	case "ecdsa-sha2-nistp256", ssh.CertAlgoECDSA256v01:
		return []string{ssh.KeyAlgoECDSA256}
	case "ecdsa-sha2-nistp384", ssh.CertAlgoECDSA384v01:
		return []string{ssh.KeyAlgoECDSA384}
	case "ecdsa-sha2-nistp521", ssh.CertAlgoECDSA521v01:
		return []string{ssh.KeyAlgoECDSA521}
	case "ssh-rsa", ssh.CertAlgoRSAv01:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	default:
		return nil
	}
}

// isUnknownHostKey reports whether a knownhosts error means the host is not
// present in known_hosts at all (as opposed to a key mismatch).
func isUnknownHostKey(err error) bool {
	var keyErr *knownhosts.KeyError
	return errors.As(err, &keyErr) && len(keyErr.Want) == 0
}

// IsLocalHost reports whether the target identified by ip/connectionType is
// the local machine (no SSH required).
func IsLocalHost(ip, connectionType string) bool {
	return ip == "127.0.0.1" || ip == "localhost" || connectionType == "local"
}

// sshCommandTimeout bounds every remote command so a wedged sshd (or a hung
// shell) cannot hang scheduler loops and deploy goroutines forever.
// Override with PROMLIFECYCLE_SSH_TIMEOUT (seconds).
func sshCommandTimeout() time.Duration {
	if v := os.Getenv("PROMLIFECYCLE_SSH_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 120 * time.Second
}

const maxSSHCommandOutput = 1 << 20

type limitedOutputBuffer struct {
	buf       bytes.Buffer
	remaining int
	truncated bool
}

func newLimitedOutputBuffer(limit int) *limitedOutputBuffer {
	return &limitedOutputBuffer{remaining: limit}
}

func (b *limitedOutputBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	if len(p) > b.remaining {
		p = p[:b.remaining]
		b.truncated = true
	}
	if len(p) > 0 {
		_, _ = b.buf.Write(p)
		b.remaining -= len(p)
	}
	return originalLen, nil
}

func (b *limitedOutputBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[输出已截断]"
	}
	return b.buf.String()
}

func (c *Client) RunCommand(cmd string) (string, string, error) {
	session, err := c.NewSession()
	if err != nil {
		return "", "", fmt.Errorf("create session: %w", err)
	}
	defer session.Close()

	stdout := newLimitedOutputBuffer(maxSSHCommandOutput)
	stderr := newLimitedOutputBuffer(maxSSHCommandOutput)
	session.Stdout = stdout
	session.Stderr = stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(sshCommandTimeout()):
		// Closing the session interrupts the remote command.
		session.Close()
		<-done
		return stdout.String(), stderr.String(), fmt.Errorf("SSH 命令执行超时（%v），请检查远程主机状态后重试", sshCommandTimeout())
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*ssh.ExitError); ok {
			return stdout.String(), stderr.String(), fmt.Errorf("exit %d: %s", exitErr.ExitStatus(), stderr.String())
		}
		return stdout.String(), stderr.String(), runErr
	}

	return stdout.String(), stderr.String(), nil
}

// ErrRemoteTerminationUnconfirmed means callers must not start compensation:
// the SSH transport could not prove that the remote mutation stopped.
var ErrRemoteTerminationUnconfirmed = errors.New("remote command termination unconfirmed")

// RunCommandContext executes Linux mutations under GNU timeout's process-group
// watchdog. Cancellation forwards TERM to that watchdog, which kills the entire
// group after a five-second grace period. A transport failure is not proof that
// the remote command has terminated.
func (c *Client) RunCommandContext(ctx context.Context, cmd string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, sshCommandTimeout())
	defer cancel()
	deadline, _ := ctx.Deadline()
	seconds := time.Until(deadline).Seconds()
	if seconds <= 0 {
		return "", "", context.DeadlineExceeded
	}
	session, err := c.NewSession()
	if err != nil {
		return "", "", fmt.Errorf("create session: %w", err)
	}
	defer session.Close()
	stdout := newLimitedOutputBuffer(maxSSHCommandOutput)
	stderr := newLimitedOutputBuffer(maxSSHCommandOutput)
	session.Stdout, session.Stderr = stdout, stderr
	// exec makes the SSH signal recipient the watchdog, not an intermediate
	// shell. timeout without --foreground owns and signals the command group.
	wrapped := fmt.Sprintf("command -v timeout >/dev/null 2>&1 || exit 125; exec timeout --signal=TERM --kill-after=5s %.3fs sh -c %s", seconds, utils.QuoteShell(cmd))
	if err := session.Start(wrapped); err != nil {
		return "", "", fmt.Errorf("%w: start: %v", ErrRemoteTerminationUnconfirmed, err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGTERM)
		select {
		case runErr = <-done:
		case <-time.After(7 * time.Second):
			_ = session.Close()
			<-done
			return stdout.String(), stderr.String(), fmt.Errorf("%w: %v", ErrRemoteTerminationUnconfirmed, ctx.Err())
		}
	}
	if runErr != nil {
		var exitErr *ssh.ExitError
		if !errors.As(runErr, &exitErr) {
			return stdout.String(), stderr.String(), fmt.Errorf("%w: %v", ErrRemoteTerminationUnconfirmed, runErr)
		}
		// A killed watchdog (137) cannot report that all descendants exited.
		// This includes the kill-after path: conservatively prohibit recovery.
		if exitErr.ExitStatus() == 124 || exitErr.ExitStatus() == 137 || exitErr.ExitStatus() < 0 {
			return stdout.String(), stderr.String(), fmt.Errorf("%w: %v", ErrRemoteTerminationUnconfirmed, runErr)
		}
		if ctx.Err() != nil {
			return stdout.String(), stderr.String(), fmt.Errorf("%w: %v", ErrRemoteTerminationUnconfirmed, ctx.Err())
		}
		return stdout.String(), stderr.String(), fmt.Errorf("exit %d: %s", exitErr.ExitStatus(), stderr.String())
	}
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("%w: %v", ErrRemoteTerminationUnconfirmed, ctx.Err())
	}
	return stdout.String(), stderr.String(), nil
}

func (c *Client) CheckDocker() (bool, error) {
	_, _, err := c.RunCommand("docker --version")
	if err != nil {
		return false, fmt.Errorf("docker not found")
	}
	_, _, err = c.RunCommand("docker ps")
	if err != nil {
		return false, fmt.Errorf("docker not running")
	}
	return true, nil
}

// SystemInfo holds detected system information
type SystemInfo struct {
	OSType    string // "linux" or "windows"
	OSVersion string
	IsWindows bool
}

// DetectOS runs system commands to detect the operating system and version
func (c *Client) DetectOS() (*SystemInfo, error) {
	// Try uname -a first for Linux
	output, _, err := c.RunCommand("uname -a")
	if err == nil {
		info := &SystemInfo{OSType: "linux", IsWindows: false}
		// Parse version info from uname output
		output = strings.TrimSpace(output)
		parts := strings.Fields(output)
		if len(parts) >= 3 {
			// uname -a format: Linux hostname version ...
			info.OSVersion = parts[0] + " " + parts[2]
		}
		return info, nil
	}

	// Try Windows commands (in case SSH is available on Windows)
	// Check if cmd.exe or powershell is available
	winOut, _, err := c.RunCommand("powershell -Command \"$env:OS\"")
	if err == nil {
		info := &SystemInfo{OSType: "windows", IsWindows: true}
		info.OSVersion = strings.TrimSpace(winOut)
		return info, nil
	}

	// Try ver command
	verOut, _, err := c.RunCommand("ver")
	if err == nil {
		info := &SystemInfo{OSType: "windows", IsWindows: true}
		info.OSVersion = strings.TrimSpace(verOut)
		return info, nil
	}

	return nil, fmt.Errorf("OS detection commands failed: %w", err)
}

func (c *Client) CopyFile(src io.Reader, dst string, mode int64, size int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), sshCommandTimeout())
	defer cancel()
	return c.CopyFileContext(ctx, src, dst, mode, size)
}

// CopyFileContext streams to a temporary file in the destination directory and
// atomically renames it into place. Both caller cancellation and the configured
// SSH timeout interrupt a stuck transfer.
func (c *Client) CopyFileContext(ctx context.Context, src io.Reader, dst string, mode int64, size int64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sshCommandTimeout())
		defer cancel()
	}
	session, err := c.NewSession()
	if err != nil {
		return fmt.Errorf("创建 SSH 文件会话失败，请重试: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("创建 SSH 输入流失败，请重试: %w", err)
	}
	tmp := fmt.Sprintf("%s.promlifecycle-tmp-%d", dst, time.Now().UnixNano())
	sizeCheck := "true"
	if size >= 0 {
		sizeCheck = fmt.Sprintf("[ \"$(wc -c < %s)\" -eq %d ]", utils.QuoteShell(tmp), size)
	}
	cmd := fmt.Sprintf(
		"trap 'rm -f %s' EXIT HUP INT TERM; cat > %s && %s && chmod %o %s && mv -f %s %s; status=$?; trap - EXIT; rm -f %s; exit $status",
		utils.QuoteShell(tmp), utils.QuoteShell(tmp), sizeCheck, mode, utils.QuoteShell(tmp),
		utils.QuoteShell(tmp), utils.QuoteShell(dst), utils.QuoteShell(tmp),
	)
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("启动 SSH 文件写入失败，请检查目标目录权限: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		var copyErr error
		if size >= 0 {
			var written int64
			written, copyErr = io.Copy(stdin, io.LimitReader(src, size))
			if copyErr == nil && written != size {
				copyErr = fmt.Errorf("文件数据不完整：期望 %d 字节，实际 %d 字节", size, written)
			}
		} else {
			_, copyErr = io.Copy(stdin, src)
		}
		closeErr := stdin.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr == nil {
			copyErr = session.Wait()
		}
		done <- copyErr
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("远程原子写入失败，原文件未替换；请检查磁盘空间和目录权限: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = session.Close()
		return fmt.Errorf("远程文件传输超时或已取消，原文件未替换；请检查网络后重试: %w", ctx.Err())
	}
}
