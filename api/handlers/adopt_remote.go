package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
)

// Contents stay internal: configuration and unit files can contain credentials.
type remoteTakeoverEvidence struct {
	ConfigPath    string `json:"config_path"`
	DataPath      string `json:"data_path"`
	ConfigContent []byte `json:"config_content"`
	UnitContent   []byte `json:"unit_content"`
	Unit          string `json:"unit"`
	ExecStart     string `json:"exec_start"`
	Error         string `json:"error"`
}

func remoteTakeoverConfigName(componentType string) string {
	switch componentType {
	case "prometheus":
		return "prometheus.yml"
	case "alertmanager":
		return "alertmanager.yml"
	case "blackbox_exporter":
		return "blackbox.yml"
	default:
		return ""
	}
}

func probeRemoteTakeover(ctx context.Context, host *models.Host, selected *hostProcessEvidence) (*remoteTakeoverEvidence, error) {
	item := selected.component
	if item.Type == "prometheus" {
		config, data, err := deployer.PrometheusArgvPaths(selected.args)
		if err != nil {
			return nil, fmt.Errorf("原 Prometheus 启动参数不满足生命周期管理要求: %w", err)
		}
		if config != item.Config || data != item.Data {
			return nil, fmt.Errorf("原启动参数与发现的配置/数据路径不一致")
		}
	}
	switch item.Type {
	case "prometheus", "alertmanager":
		if item.Config == "" || item.Data == "" {
			return nil, fmt.Errorf("远程托管 %s 需要原启动参数明确指定配置文件和数据目录的绝对路径；请先规范原服务或使用 observe", item.Type)
		}
	case "blackbox_exporter":
		if item.Config == "" {
			return nil, fmt.Errorf("远程托管 blackbox_exporter 需要明确的配置文件绝对路径")
		}
	case "node_exporter":
	default:
		return nil, fmt.Errorf("%s 的远程原服务布局尚不能完整核实，请使用 observe；远程托管支持 Prometheus、Alertmanager、node_exporter 和 blackbox_exporter", item.Type)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.service$`).MatchString(item.Unit) {
		return nil, fmt.Errorf("无法安全识别原 systemd 服务单元")
	}
	for _, path := range []string{item.Binary, item.Config, item.Data} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n")) {
			return nil, fmt.Errorf("原服务的二进制、配置和数据路径必须是规范的绝对路径；请先规范原服务或使用 observe")
		}
	}
	if !utils.SafeDataPath(item.Data) {
		return nil, fmt.Errorf("原数据目录不满足生命周期管理的独立目录要求")
	}
	if item.Config != "" && item.Data != "" && (item.Config == item.Data || strings.HasPrefix(item.Config, item.Data+"/") || strings.HasPrefix(item.Data, item.Config+"/")) {
		return nil, fmt.Errorf("原配置和数据必须是互不重叠的独立路径")
	}
	input, err := json.Marshal(map[string]interface{}{
		"host_id": host.ID, "type": item.Type, "pid": item.PID, "port": item.Port,
		"binary": item.Binary, "config": item.Config, "data": item.Data,
		"unit": item.Unit, "fingerprint": item.Fingerprint, "args": selected.args,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	client, err := ssh.NewClientContext(ctx, host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("远程接管 SSH 连接失败，请检查主机连接")
	}
	defer client.Close()
	command := "# promlifecycle-remote-takeover-probe\npython3 - " + utils.QuoteShell(base64.StdEncoding.EncodeToString(input)) + " <<'PLC_REMOTE_TAKEOVER'\n" + remoteTakeoverProbe + "\nPLC_REMOTE_TAKEOVER"
	output, _, err := client.RunCommand(command)
	if err != nil {
		return nil, fmt.Errorf("远程接管预检失败，需要 root SSH、Python 3 和可用的 systemd；原服务未修改")
	}
	var evidence remoteTakeoverEvidence
	if err := json.Unmarshal([]byte(output), &evidence); err != nil {
		return nil, fmt.Errorf("远程接管预检返回无效或超限的证据")
	}
	if evidence.Error != "" {
		return nil, fmt.Errorf("%s", evidence.Error)
	}
	if evidence.Unit != item.Unit || evidence.ConfigPath != item.Config || evidence.DataPath != item.Data || len(bytes.TrimSpace(evidence.UnitContent)) == 0 || (item.Config != "" && len(evidence.ConfigContent) == 0) {
		return nil, fmt.Errorf("远程接管证据与发现结果不一致，请重新扫描")
	}
	if err := services.ValidateAdoptedServiceLaunch(evidence.ExecStart, item.Binary, selected.args); err != nil {
		return nil, fmt.Errorf("原 systemd 启动参数无法与当前进程严格对应；请先完成原服务变更，或使用 observe")
	}
	if err := validateRemoteTakeoverHealth(ctx, host, item.Type, item.Port); err != nil {
		return nil, err
	}
	return &evidence, nil
}

func validateRemoteTakeoverHealth(ctx context.Context, host *models.Host, componentType string, port int) error {
	path := "/metrics"
	if componentType == "prometheus" || componentType == "alertmanager" {
		path = "/-/ready"
	}
	if componentType == "blackbox_exporter" {
		path = "/-/healthy"
	}
	endpoint := "http://" + net.JoinHostPort(host.IP, strconv.Itoa(port)) + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("无法构造目标主机健康端点")
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("平台无法访问目标主机健康端点，请检查监听地址和网络，或使用 observe")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("目标主机健康端点未就绪，请检查服务访问配置，或使用 observe")
	}
	return nil
}

// A read-only probe. Identity is checked both before and after capturing files.
// Keep the combined base64 output below the SSH client's 1 MiB output limit.
// Python 3.6 is intentionally supported on existing Enterprise Linux hosts.
const remoteTakeoverProbe = `import base64, hashlib, json, os, re, stat, subprocess, sys
q = json.loads(base64.b64decode(sys.argv[1]).decode('utf-8'))
def require(ok, message):
    if not ok: raise ValueError(message)
def command(args):
    return subprocess.check_output(args, stderr=subprocess.DEVNULL, timeout=10).decode('utf-8')
def bounded_read(path, limit):
    with open(path, 'rb') as f:
        require(stat.S_ISREG(os.fstat(f.fileno()).st_mode), '原配置必须是普通文件')
        data = f.read(limit + 1)
    require(len(data) <= limit, '原配置或服务单元超过接管大小上限（配置 512 KiB、单元 64 KiB）')
    return data
def identity():
    p = '/proc/' + str(q['pid'])
    exe = os.readlink(p + '/exe')
    with open(p + '/stat') as f: s = f.read()
    start = s[s.rfind(')')+1:].split()[19]
    raw = '{}:{}:{}:{}:{}:{}'.format(q['host_id'], q['type'], q['pid'], start, q['port'], exe)
    require(hashlib.sha256(raw.encode()).hexdigest() == q['fingerprint'], '原进程身份已变化，请重新扫描')
    for path in (q['binary'], q['config'], q['data']):
        if path: require(os.path.samefile(p + '/root' + path, path), '原进程使用独立文件系统中的路径，无法保证托管操作作用于同一文件，请使用 observe')
    with open(p + '/cmdline', 'rb') as f: argv = f.read().rstrip(b'\0').split(b'\0')
    require([x.decode('utf-8') for x in argv[1:]] == q['args'], '原启动参数已变化，请重新扫描')
    props = dict(line.split('=', 1) for line in command(['systemctl', 'show', q['unit'], '--property=MainPID,LoadState,ActiveState,FragmentPath,NeedDaemonReload,UnitFileState']).splitlines() if '=' in line)
    require(props.get('MainPID') == str(q['pid']), 'systemd 主进程已变化，请重新扫描')
    require(props.get('LoadState') == 'loaded' and props.get('ActiveState') == 'active', '原 systemd 服务必须已加载且正在运行')
    require(props.get('UnitFileState') == 'enabled', '远程托管需要已启用开机启动的 systemd 单元，以保证停机后的历史恢复可核实；请先规范原服务或使用 observe')
    require(props.get('NeedDaemonReload') == 'no', '原服务单元存在尚未加载的修改，请先完成原服务变更')
    require(os.path.isabs(props.get('FragmentPath', '')), '无法定位原服务单元文件')
    listeners = command(['ss', '-H', '-ltnp'])
    require(any(len(line.split()) >= 5 and line.split()[3].rsplit(':', 1)[-1] == str(q['port']) and re.search(r'pid=' + str(q['pid']) + r'[,)]', line) for line in listeners.splitlines()), '原监听端口已变化，请重新扫描')
def capture():
    identity()
    for path in (q['binary'], q['config'], q['data']):
        if path:
            require(os.path.realpath(path) == path, '原服务路径含符号链接，不能保证后续升级写入同一位置，请使用 observe')
            require(os.path.exists(path), '原服务路径不存在')
    require(os.path.isfile(q['binary']) and os.access(q['binary'], os.X_OK), '原二进制不可执行')
    if q['data']: require(os.path.isdir(q['data']), '原数据路径不是目录')
    config = bounded_read(q['config'], 512 * 1024) if q['config'] else b''
    unit = command(['systemctl', 'cat', '--', q['unit']]).encode('utf-8')
    require(0 < len(unit) <= 64 * 1024, '原服务单元超过 64 KiB 接管上限或内容为空')
    escaped = ''.join(c if ('a' <= c <= 'z' or 'A' <= c <= 'Z' or (i > 0 and '0' <= c <= '9')) else '_{:02x}'.format(ord(c)) for i, c in enumerate(q['unit']))
    busargs = ['busctl', 'get-property', 'org.freedesktop.systemd1', '/org/freedesktop/systemd1/unit/' + escaped, 'org.freedesktop.systemd1.Service', 'ExecStart']
    launch = command(busargs)
    require(0 < len(launch) <= 64 * 1024, '原 systemd 启动参数超过接管上限或为空')
    identity()
    require(not q['config'] or bounded_read(q['config'], 512 * 1024) == config, '读取期间原配置已变化，请重试')
    require(command(['systemctl', 'cat', '--', q['unit']]).encode('utf-8') == unit, '读取期间原服务单元已变化，请重试')
    require(command(busargs) == launch, '读取期间原 systemd 启动参数已变化，请重试')
    return dict(config_path=q['config'], data_path=q['data'], unit=q['unit'], exec_start=launch, config_content=base64.b64encode(config).decode(), unit_content=base64.b64encode(unit).decode())
try:
    require(os.geteuid() == 0, '远程托管需要 root SSH 权限')
    print(json.dumps(capture()))
except ValueError as e:
    print(json.dumps(dict(error=str(e))))
except Exception:
    print(json.dumps(dict(error='无法核实原进程、systemd 单元或读取原文件；原服务未修改，请重新扫描并检查主机权限')))
`
