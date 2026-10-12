package deployer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	winrmclient "github.com/promlifecycle/platform/core/winrm"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

const maxWindowsPackageSize int64 = 512 * 1024 * 1024

var windowsCollectorName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type windowsExporterConfig struct {
	Collectors    []string `json:"collectors"`
	RemoteAddress string   `json:"remote_address"`
}

// WindowsDeployer manages windows_exporter as an MSI-installed Windows service
// through WinRM. It deliberately remains separate from the Linux/systemd
// binary deployer; only the lifecycle contract is shared.
type WindowsDeployer struct {
	db       *gorm.DB
	basePath string
	progress ProgressReporter
}

func NewWindowsDeployer(db *gorm.DB, basePath string) *WindowsDeployer {
	return &WindowsDeployer{db: db, basePath: basePath}
}

func (d *WindowsDeployer) WithProgress(reporter ProgressReporter) *WindowsDeployer {
	d.progress = reporter
	return d
}

func (d *WindowsDeployer) client(host *models.Host) (*winrmclient.Client, error) {
	password, err := credentials.Decrypt(host.Password)
	if err != nil {
		return nil, fmt.Errorf("decrypt WinRM credential: %w", err)
	}
	return winrmclient.NewClientWithOptions(winrmclient.Options{
		Host: host.IP, Port: host.WinRMPort, Scheme: host.WinRMScheme,
		Username: host.Username, Password: password, SkipTLSVerify: host.WinRMSkipVerify,
	})
}

func (d *WindowsDeployer) Deploy(ctx context.Context, component *models.Component) error {
	reportProgress(d.progress, "prechecking", "检查 Windows 主机、WinRM 和部署参数", 5)
	if component == nil || component.Host.ID == 0 {
		return fmt.Errorf("Windows deployment requires a loaded host")
	}
	if component.Type != models.ComponentWindowsExporter {
		return fmt.Errorf("Windows deployer does not support %s", component.Type)
	}
	if component.Host.ConnectionType != "winrm" || component.Host.OS != "windows" {
		return fmt.Errorf("windows_exporter requires a Windows WinRM host")
	}
	if component.Port == 0 {
		component.Port = 9182
	}
	if component.Port < 1 || component.Port > 65535 {
		return fmt.Errorf("windows_exporter port is invalid")
	}

	config, err := parseWindowsExporterConfig(component.Config)
	if err != nil {
		return err
	}
	if config.RemoteAddress == "" {
		config.RemoteAddress = d.prometheusAddress(component.ClusterID, component.Host.IP)
	}

	reportProgress(d.progress, "resolving_package", "解析并校验 Windows Exporter MSI", 15)
	packagePath, checksum, err := d.packageFor(component.Version)
	if err != nil {
		return err
	}
	client, err := d.client(&component.Host)
	if err != nil {
		return err
	}
	reportProgress(d.progress, "connecting", "通过 WinRM 验证管理员权限和系统架构", 25)

	if _, err := client.RunPS(ctx, windowsPreflightScript); err != nil {
		return fmt.Errorf("Windows deployment preflight failed: %w", err)
	}
	reportProgress(d.progress, "uploading", "通过 WinRM 上传 Windows Exporter MSI", 40)

	remoteDir := `C:\Windows\Temp\PromLifecycle`
	remotePackage := remoteDir + `\windows_exporter-` + component.Version + `.msi`
	if _, err := client.RunPS(ctx, fmt.Sprintf("New-Item -ItemType Directory -Force -Path %s | Out-Null", quotePowerShell(remoteDir))); err != nil {
		return fmt.Errorf("create Windows staging directory: %w", err)
	}
	if err := client.UploadFile(ctx, packagePath, remotePackage); err != nil {
		return fmt.Errorf("upload windows_exporter MSI: %w", err)
	}
	verifyScript := fmt.Sprintf(
		"$actual=(Get-FileHash -Algorithm SHA256 -Path %s).Hash.ToLowerInvariant(); if($actual -ne %s){ throw ('MSI checksum mismatch: '+$actual) }; Write-Output $actual",
		quotePowerShell(remotePackage), quotePowerShell(strings.ToLower(checksum)),
	)
	if _, err := client.RunPS(ctx, verifyScript); err != nil {
		return fmt.Errorf("verify windows_exporter MSI: %w", err)
	}
	reportProgress(d.progress, "installing", "静默安装 MSI 并启动 Windows 服务", 65)

	collectors := strings.Join(config.Collectors, ",")
	installScript := fmt.Sprintf(`
$arguments=@('/i',%s,'/qn','/norestart',%s,%s)
$process=Start-Process -FilePath 'msiexec.exe' -ArgumentList $arguments -Wait -PassThru
if($process.ExitCode -notin @(0,3010)){ throw ('msiexec failed with exit code '+$process.ExitCode) }
$service=Get-Service -Name 'windows_exporter' -ErrorAction Stop
if($service.Status -ne 'Running'){ Start-Service -Name 'windows_exporter' }
Write-Output ('msiexec_exit_code='+$process.ExitCode)
`, quotePowerShell(remotePackage), quotePowerShell("ENABLED_COLLECTORS="+collectors), quotePowerShell("LISTEN_PORT="+strconv.Itoa(component.Port)))
	if _, err := client.RunPS(ctx, installScript); err != nil {
		return fmt.Errorf("install windows_exporter: %w", err)
	}
	reportProgress(d.progress, "configuring_firewall", "配置 Windows 防火墙允许 Prometheus 抓取", 75)

	if config.RemoteAddress != "" {
		firewallScript := fmt.Sprintf(`
$name='PromLifecycle windows_exporter'
Get-NetFirewallRule -DisplayName $name -ErrorAction SilentlyContinue | Remove-NetFirewallRule
New-NetFirewallRule -DisplayName $name -Direction Inbound -Action Allow -Protocol TCP -LocalPort %d -RemoteAddress %s | Out-Null
`, component.Port, quotePowerShell(config.RemoteAddress))
		if _, err := client.RunPS(ctx, firewallScript); err != nil {
			return fmt.Errorf("configure scoped Windows firewall rule: %w", err)
		}
	}

	component.ServiceName = "windows_exporter"
	component.InstallPath = `C:\Program Files\windows_exporter`
	component.InternalURL = fmt.Sprintf("http://%s:%d", component.Host.IP, component.Port)
	encodedConfig, _ := json.Marshal(config)
	component.Config = string(encodedConfig)
	reportProgress(d.progress, "verifying", "验证 Windows 服务和本机 metrics 端点", 85)
	if err := d.HealthCheck(ctx, component); err != nil {
		return err
	}
	reportProgress(d.progress, "integrating", "Windows Exporter 验证通过，准备同步 Prometheus", 95)
	_, _ = client.RunPS(ctx, "Remove-Item -Force -ErrorAction SilentlyContinue "+quotePowerShell(remotePackage))
	return nil
}

func (d *WindowsDeployer) Start(ctx context.Context, component *models.Component) error {
	return d.serviceAction(ctx, component, "Start-Service -Name 'windows_exporter'; (Get-Service -Name 'windows_exporter').Status")
}

func (d *WindowsDeployer) Stop(ctx context.Context, component *models.Component) error {
	return d.serviceAction(ctx, component, "Stop-Service -Name 'windows_exporter' -Force; (Get-Service -Name 'windows_exporter').Status")
}

func (d *WindowsDeployer) Restart(ctx context.Context, component *models.Component) error {
	return d.serviceAction(ctx, component, "Restart-Service -Name 'windows_exporter' -Force; (Get-Service -Name 'windows_exporter').Status")
}

func (d *WindowsDeployer) serviceAction(ctx context.Context, component *models.Component, script string) error {
	client, err := d.client(&component.Host)
	if err != nil {
		return err
	}
	if _, err := client.RunPS(ctx, script); err != nil {
		return fmt.Errorf("manage windows_exporter service: %w", err)
	}
	return nil
}

func (d *WindowsDeployer) HealthCheck(ctx context.Context, component *models.Component) error {
	client, err := d.client(&component.Host)
	if err != nil {
		return err
	}
	script := fmt.Sprintf(`
$service=Get-Service -Name 'windows_exporter' -ErrorAction Stop
if($service.Status -ne 'Running'){ throw ('windows_exporter service is '+$service.Status) }
$response=Invoke-WebRequest -UseBasicParsing -TimeoutSec 15 -Uri %s
if($response.StatusCode -ne 200 -or $response.Content -notmatch 'windows_exporter'){ throw 'windows_exporter metrics endpoint validation failed' }
Write-Output 'healthy'
`, quotePowerShell(fmt.Sprintf("http://127.0.0.1:%d/metrics", component.Port)))
	if _, err := client.RunPS(ctx, script); err != nil {
		return fmt.Errorf("windows_exporter health check failed: %w", err)
	}
	return nil
}

func (d *WindowsDeployer) GetLogs(ctx context.Context, component *models.Component, lines int) ([]string, error) {
	if lines <= 0 || lines > 500 {
		lines = 100
	}
	client, err := d.client(&component.Host)
	if err != nil {
		return nil, err
	}
	script := fmt.Sprintf(`Get-WinEvent -FilterHashtable @{LogName='Application'} -MaxEvents %d -ErrorAction SilentlyContinue | Where-Object {$_.ProviderName -match 'windows_exporter'} | ForEach-Object {$_.TimeCreated.ToString('s')+' '+$_.LevelDisplayName+' '+$_.Message}`, lines)
	output, err := client.RunPS(ctx, script)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(output) == "" {
		return []string{}, nil
	}
	return strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n"), nil
}

func (d *WindowsDeployer) Remove(ctx context.Context, component *models.Component) error {
	client, err := d.client(&component.Host)
	if err != nil {
		return err
	}
	script := `
$roots=@('HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*','HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*')
$product=Get-ItemProperty $roots -ErrorAction SilentlyContinue | Where-Object {$_.DisplayName -eq 'windows_exporter'} | Select-Object -First 1
if($product){
  $process=Start-Process -FilePath 'msiexec.exe' -ArgumentList @('/x',$product.PSChildName,'/qn','/norestart') -Wait -PassThru
  if($process.ExitCode -notin @(0,1605,3010)){ throw ('msiexec uninstall failed with exit code '+$process.ExitCode) }
}
Get-NetFirewallRule -DisplayName 'PromLifecycle windows_exporter' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue 'C:\Program Files\windows_exporter'
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue 'C:\Windows\Temp\PromLifecycle'
`
	if _, err := client.RunPS(ctx, script); err != nil {
		return fmt.Errorf("uninstall windows_exporter: %w", err)
	}
	return nil
}

func (d *WindowsDeployer) packageFor(version string) (string, string, error) {
	var pkg models.Package
	query := d.db.Where("type = ? AND format = ?", string(models.ComponentWindowsExporter), "msi")
	if version != "" {
		query = query.Where("version = ? OR version = ?", version, strings.TrimPrefix(version, "v"))
	}
	if err := query.Order("created_at DESC").First(&pkg).Error; err != nil {
		return "", "", fmt.Errorf("windows_exporter MSI package %q is not available offline", version)
	}
	packageDir := filepath.Join(d.basePath, "packages")
	if !utils.PathWithinBase(packageDir, pkg.StoragePath) {
		return "", "", fmt.Errorf("windows_exporter package path is outside package storage")
	}
	info, err := os.Lstat(pkg.StoragePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("windows_exporter MSI package file is unavailable")
	}
	if info.Size() > maxWindowsPackageSize {
		return "", "", fmt.Errorf("windows_exporter MSI exceeds the 512 MiB WinRM upload limit")
	}
	checksum, err := fileSHA256(pkg.StoragePath)
	if err != nil {
		return "", "", err
	}
	if pkg.Checksum != "" && !strings.EqualFold(pkg.Checksum, checksum) {
		return "", "", fmt.Errorf("stored windows_exporter MSI checksum does not match its package record")
	}
	return pkg.StoragePath, checksum, nil
}

func (d *WindowsDeployer) prometheusAddress(clusterID uint, targetHost string) string {
	var component models.Component
	if err := d.db.Preload("Host").Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentPrometheus, models.StatusRunning).First(&component).Error; err == nil {
		if ip := net.ParseIP(component.Host.IP); ip != nil && !ip.IsLoopback() {
			return ip.String()
		}
	}
	var cluster models.Cluster
	if err := d.db.First(&cluster, clusterID).Error; err == nil {
		if ip := net.ParseIP(cluster.Host); ip != nil && !ip.IsLoopback() {
			return ip.String()
		}
	}
	connection, err := net.DialTimeout("udp", net.JoinHostPort(targetHost, "9"), 3*time.Second)
	if err != nil {
		return ""
	}
	defer connection.Close()
	if address, ok := connection.LocalAddr().(*net.UDPAddr); ok && address.IP != nil && !address.IP.IsUnspecified() {
		return address.IP.String()
	}
	return ""
}

func parseWindowsExporterConfig(raw string) (windowsExporterConfig, error) {
	config := windowsExporterConfig{
		Collectors: []string{"cpu", "logical_disk", "memory", "net", "os", "physical_disk", "service", "system"},
	}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return config, fmt.Errorf("invalid windows_exporter configuration: %w", err)
		}
	}
	if len(config.Collectors) == 0 {
		return config, fmt.Errorf("at least one windows_exporter collector is required")
	}
	seen := make(map[string]bool, len(config.Collectors))
	for _, collector := range config.Collectors {
		if !windowsCollectorName.MatchString(collector) || seen[collector] {
			return config, fmt.Errorf("invalid or duplicate windows_exporter collector %q", collector)
		}
		seen[collector] = true
	}
	if config.RemoteAddress != "" {
		if ip := net.ParseIP(config.RemoteAddress); ip == nil {
			_, network, err := net.ParseCIDR(config.RemoteAddress)
			if err != nil || network == nil {
				return config, fmt.Errorf("windows_exporter firewall source must be an IP or CIDR")
			}
		}
	}
	return config, nil
}

func quotePowerShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

const windowsPreflightScript = `
$identity=[Security.Principal.WindowsIdentity]::GetCurrent()
$principal=New-Object Security.Principal.WindowsPrincipal($identity)
if(-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)){ throw 'WinRM account is not an administrator' }
if(-not [Environment]::Is64BitOperatingSystem){ throw 'windows_exporter requires a 64-bit Windows operating system' }
$os=Get-CimInstance Win32_OperatingSystem
[PSCustomObject]@{ComputerName=$env:COMPUTERNAME;Caption=$os.Caption;Version=$os.Version;Architecture=$env:PROCESSOR_ARCHITECTURE;Identity=$identity.Name} | ConvertTo-Json -Compress
`
