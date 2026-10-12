package winrm

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	masterwinrm "github.com/masterzen/winrm"
)

// Options controls a WinRM endpoint. NTLM is used for local, workgroup and
// domain credentials; HTTPS certificate verification is enabled by default.
type Options struct {
	Host           string
	Port           int
	Scheme         string
	Username       string
	Password       string
	SkipTLSVerify  bool
	RequestTimeout time.Duration
	// Context bounds all HTTP authentication and SOAP exchanges, including
	// CreateShell, which the upstream RunPSWithContext does not cover.
	Context context.Context
}

// Client executes PowerShell commands over WinRM.
type Client struct {
	c    *masterwinrm.Client
	host string
}

// NewClient keeps the existing call sites concise while using NTLM and strict
// TLS verification. Call NewClientWithOptions for an explicitly approved
// self-signed endpoint.
func NewClient(host string, port int, scheme, user, password string) (*Client, error) {
	return NewClientWithOptions(Options{
		Host: host, Port: port, Scheme: scheme, Username: user, Password: password,
	})
}

func NewClientWithOptions(options Options) (*Client, error) {
	options.Scheme = strings.ToLower(strings.TrimSpace(options.Scheme))
	if options.Scheme == "" {
		options.Scheme = "https"
	}
	if options.Scheme != "http" && options.Scheme != "https" {
		return nil, fmt.Errorf("winrm scheme must be http or https")
	}
	if strings.TrimSpace(options.Host) == "" || strings.TrimSpace(options.Username) == "" {
		return nil, fmt.Errorf("winrm host and username are required")
	}
	if options.Port == 0 {
		if options.Scheme == "https" {
			options.Port = 5986
		} else {
			options.Port = 5985
		}
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 60 * time.Second
	}

	endpoint := masterwinrm.NewEndpoint(
		options.Host,
		options.Port,
		options.Scheme == "https",
		options.SkipTLSVerify,
		nil,
		nil,
		nil,
		options.RequestTimeout,
	)
	encryptedTransport, err := masterwinrm.NewEncryption("ntlm")
	if err != nil {
		return nil, fmt.Errorf("winrm encrypted transport: %w", err)
	}
	if options.Context != nil {
		encryptedTransport.SetContext(options.Context)
	}
	params := *masterwinrm.DefaultParameters
	params.TransportDecorator = func() masterwinrm.Transporter {
		return encryptedTransport
	}
	client, err := masterwinrm.NewClientWithParameters(endpoint, options.Username, options.Password, &params)
	if err != nil {
		return nil, fmt.Errorf("winrm client: %w", err)
	}
	return &Client{c: client, host: options.Host}, nil
}

// RunPS executes a PowerShell command and returns stdout and stderr together.
func (c *Client) RunPS(ctx context.Context, command string) (string, error) {
	stdout, stderr, code, err := c.c.RunPSWithContext(ctx, command)
	output := strings.TrimSpace(stdout + stderr)
	if err != nil {
		return output, err
	}
	if code != 0 {
		return output, fmt.Errorf("powershell exit code %d: %s", code, output)
	}
	return output, nil
}

// UploadFile publishes one immutable local file through the platform's
// short-lived, single-use transfer endpoint. The URL token travels only inside
// encrypted WinRM; Windows then downloads the MSI over the platform port.
func (c *Client) UploadFile(ctx context.Context, localPath, remotePath string) error {
	token, transfer, cleanup, err := registerFileTransfer(localPath, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("register WinRM file transfer: %w", err)
	}
	defer cleanup()

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PROMLIFECYCLE_TRANSFER_BASE_URL")), "/")
	if baseURL == "" {
		sourceIP, routeErr := outboundIP(c.host)
		if routeErr != nil {
			return fmt.Errorf("resolve WinRM transfer address: %w", routeErr)
		}
		port := 8080
		if configured := strings.TrimSpace(os.Getenv("PROMLIFECYCLE_PORT")); configured != "" {
			if parsed, parseErr := strconv.Atoi(configured); parseErr == nil && parsed > 0 && parsed <= 65535 {
				port = parsed
			}
		}
		baseURL = "http://" + net.JoinHostPort(sourceIP.String(), strconv.Itoa(port))
	}
	downloadURL := baseURL + "/api/v1/winrm-transfers/" + token
	quotedPath := strings.ReplaceAll(remotePath, "'", "''")
	quotedURL := strings.ReplaceAll(downloadURL, "'", "''")
	if _, err = c.RunPS(ctx, fmt.Sprintf(
		"Invoke-WebRequest -UseBasicParsing -TimeoutSec 300 -Uri '%s' -OutFile '%s'; if(-not (Test-Path -LiteralPath '%s')){ throw 'downloaded file missing' }",
		quotedURL, quotedPath, quotedPath,
	)); err != nil {
		return fmt.Errorf("Windows host pull upload: %w", err)
	}
	select {
	case <-transfer.done:
		return nil
	default:
		return fmt.Errorf("Windows host completed without downloading the file")
	}
}

func outboundIP(host string) (net.IP, error) {
	connection, err := net.DialTimeout("udp", net.JoinHostPort(host, "9"), 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	address, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || address.IP == nil || address.IP.IsUnspecified() {
		return nil, fmt.Errorf("no routable local address")
	}
	return address.IP, nil
}
