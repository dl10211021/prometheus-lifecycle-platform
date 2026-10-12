package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
)

var deploymentParameterTypes = map[string]bool{
	"package-version": true, "number": true, "path": true, "text": true,
	"textarea": true, "password": true, "boolean": true, "select": true,
	"multiselect": true, "duration": true, "size": true, "url": true, "regex": true,
}

var customDeploymentFields = map[string]bool{
	"version": true, "port": true, "install_path": true,
	"data_path_override": true, "custom_args": true,
	"custom_env": true, "resource_limits": true,
}

var customDeploymentFieldTypes = map[string]string{
	"version": "package-version", "port": "number", "install_path": "path",
	"data_path_override": "path", "custom_args": "textarea",
	"custom_env": "textarea", "resource_limits": "textarea",
}

func (h *Handler) ListDeploymentDefinitions(c *gin.Context) {
	definitions := builtInDeploymentDefinitions()
	var custom []models.ComponentDefinition
	if err := h.db.Where("enabled = ?", true).Order("type ASC").Find(&custom).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取自定义部署定义失败"})
		return
	}
	for _, definition := range custom {
		parameters := customDefinitionParameters(definition)
		definitions = append(definitions, models.DeploymentDefinition{
			Type: definition.Type, DisplayName: definition.DisplayName,
			Description: "自定义单二进制组件", Platform: "linux", DeployType: "binary",
			ServiceManager: "systemd", HealthPath: definition.HealthPath,
			BuiltIn: false, Parameters: parameters,
		})
	}
	c.JSON(http.StatusOK, gin.H{"definitions": definitions})
}

func builtInDeploymentDefinitions() []models.DeploymentDefinition {
	types := []struct {
		component                                                                           models.ComponentType
		name, description, platform, deployType, manager, healthPath, installPath, dataPath string
		port                                                                                int
	}{
		{models.ComponentPrometheus, "Prometheus", "指标采集、存储与查询核心", "linux", "binary", "systemd", "/-/ready", "/opt/promlifecycle/components/prometheus", "/var/lib/promlifecycle/prometheus", 9090},
		{models.ComponentAlertmanager, "Alertmanager", "告警分组、路由与通知", "linux", "binary", "systemd", "/-/ready", "/opt/promlifecycle/components/alertmanager", "/var/lib/promlifecycle/alertmanager", 9093},
		{models.ComponentGrafana, "Grafana", "指标查询与仪表盘", "linux", "binary", "systemd", "/api/health", "/opt/promlifecycle/components/grafana", "/var/lib/promlifecycle/grafana", 3000},
		{models.ComponentNodeExporter, "Node Exporter", "Linux 主机指标采集", "linux", "binary", "systemd", "/metrics", "/opt/promlifecycle/components/node_exporter", "", 9100},
		{models.ComponentWindowsExporter, "Windows Exporter", "Windows 主机指标采集", "windows", "windows", "windows-scm", "/metrics", `C:\Program Files\windows_exporter`, "", 9182},
		{models.ComponentBlackbox, "Blackbox Exporter", "HTTP、TCP、ICMP 与 DNS 探测", "linux", "binary", "systemd", "/-/healthy", "/opt/promlifecycle/components/blackbox_exporter", "", 9115},
		{models.ComponentPushgateway, "Pushgateway", "短任务指标推送网关", "linux", "binary", "systemd", "/-/healthy", "/opt/promlifecycle/components/pushgateway", "/var/lib/promlifecycle/pushgateway", 9091},
		{models.ComponentLoki, "Loki", "日志索引与查询", "linux", "binary", "systemd", "/ready", "/opt/promlifecycle/components/loki", "/var/lib/promlifecycle/loki", 3100},
		{models.ComponentPromtail, "Promtail", "日志采集与推送", "linux", "binary", "systemd", "/ready", "/opt/promlifecycle/components/promtail", "/var/lib/promlifecycle/promtail", 9080},
		{models.ComponentThanos, "Thanos", "Prometheus Sidecar 与长期存储", "linux", "binary", "systemd", "/-/ready", "/opt/promlifecycle/components/thanos", "", 10902},
	}
	definitions := make([]models.DeploymentDefinition, 0, len(types))
	for _, item := range types {
		parameters := standardDeploymentParameters(item.port, item.installPath, item.dataPath)
		if item.component == models.ComponentPrometheus {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "retention_time", Label: "数据保留时间", Type: "duration", Required: true, Default: "30d", Help: "例如 15d、30d、12h；此参数要求 Prometheus >= 2.7.0", VersionRule: models.PrometheusArgumentVersionRule("--storage.tsdb.retention.time")},
				models.DeploymentParameter{Field: "scrape_interval", Label: "默认抓取周期", Type: "duration", Required: true, Default: "15s"},
				models.DeploymentParameter{Field: "evaluation_interval", Label: "规则计算周期", Type: "duration", Required: true, Default: "15s"},
				models.DeploymentParameter{Field: "scrape_timeout", Label: "默认抓取超时", Type: "duration", Required: true, Default: "10s", Help: "不能大于默认抓取周期"},
				models.DeploymentParameter{Field: "retention_size", Label: "存储空间上限", Type: "size", Advanced: true, Placeholder: "例如 50GB；留空表示不限制", Help: "要求 Prometheus >= 2.7.0", VersionRule: models.PrometheusArgumentVersionRule("--storage.tsdb.retention.size")},
				models.DeploymentParameter{Field: "query_timeout", Label: "查询超时", Type: "duration", Required: true, Default: "2m", Advanced: true},
				models.DeploymentParameter{Field: "query_max_concurrency", Label: "查询最大并发", Type: "number", Required: true, Default: 20, Min: 1, Max: 1000, Advanced: true},
				models.DeploymentParameter{Field: "external_url", Label: "外部访问地址", Type: "url", Advanced: true, Placeholder: "https://prometheus.example.com"},
				models.DeploymentParameter{Field: "enable_lifecycle", Label: "启用配置热加载", Type: "boolean", Required: true, Default: true, Advanced: true, Help: "开启 /-/reload，平台自动配置同步依赖此能力"},
			)
		}
		if item.component == models.ComponentAlertmanager {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "data_retention", Label: "告警与静默保留时间", Type: "duration", Required: true, Default: "120h"},
				models.DeploymentParameter{Field: "resolve_timeout", Label: "告警恢复超时", Type: "duration", Required: true, Default: "5m"},
				models.DeploymentParameter{Field: "group_wait", Label: "首次分组等待", Type: "duration", Required: true, Default: "30s"},
				models.DeploymentParameter{Field: "group_interval", Label: "分组更新周期", Type: "duration", Required: true, Default: "5m"},
				models.DeploymentParameter{Field: "repeat_interval", Label: "重复通知周期", Type: "duration", Required: true, Default: "4h"},
				models.DeploymentParameter{Field: "data_maintenance_interval", Label: "数据维护周期", Type: "duration", Required: true, Default: "15m", Advanced: true},
				models.DeploymentParameter{Field: "alerts_gc_interval", Label: "告警回收周期", Type: "duration", Required: true, Default: "30m", Advanced: true},
				models.DeploymentParameter{Field: "external_url", Label: "外部访问地址", Type: "url", Advanced: true, Placeholder: "https://alertmanager.example.com"},
				models.DeploymentParameter{Field: "route_prefix", Label: "Web 路由前缀", Type: "text", Required: true, Default: "/", Advanced: true, Help: "必须以 / 开头，不包含查询参数或片段"},
				models.DeploymentParameter{Field: "web_get_concurrency", Label: "GET 请求最大并发", Type: "number", Required: true, Default: 0, Min: 0, Max: 10000, Advanced: true, Help: "0 表示由 Alertmanager 根据 CPU 自动确定"},
				models.DeploymentParameter{Field: "web_timeout", Label: "HTTP 请求超时", Type: "duration", Advanced: true, Placeholder: "例如 30s；留空表示不限制"},
				models.DeploymentParameter{Field: "enable_cluster", Label: "启用 HA 集群通信", Type: "boolean", Required: true, Default: false, Advanced: true, Help: "单节点默认关闭 9094 Gossip 监听"},
				models.DeploymentParameter{Field: "log_level", Label: "日志级别", Type: "select", Required: true, Default: "info", Options: []string{"debug", "info", "warn", "error"}, Advanced: true},
				models.DeploymentParameter{Field: "log_format", Label: "日志格式", Type: "select", Required: true, Default: "logfmt", Options: []string{"logfmt", "json"}, Advanced: true},
			)
		}
		if item.component == models.ComponentGrafana {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "initial_password", Label: "初始管理员密码", Type: "password", Help: "首次安装必须设置；保留数据恢复时留空会读取受保护的已有凭据；不会写入部署参数快照或模板"},
				models.DeploymentParameter{Field: "domain", Label: "访问域名", Type: "text", Required: true, Default: "localhost", Help: "仅填写主机名或 IP，不包含协议和端口"},
				models.DeploymentParameter{Field: "root_url", Label: "外部访问地址", Type: "url", Advanced: true, Placeholder: "https://grafana.example.com/monitoring/"},
				models.DeploymentParameter{Field: "serve_from_sub_path", Label: "从 URL 子路径提供服务", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "default_language", Label: "默认语言", Type: "select", Required: true, Default: "zh-Hans", Options: []string{"zh-Hans", "en-US"}},
				models.DeploymentParameter{Field: "default_timezone", Label: "默认时区", Type: "text", Required: true, Default: "browser", Placeholder: "browser、utc 或 Asia/Shanghai"},
				models.DeploymentParameter{Field: "auto_assign_org_role", Label: "新用户默认角色", Type: "select", Required: true, Default: "Viewer", Options: []string{"Viewer", "Editor", "Admin"}},
				models.DeploymentParameter{Field: "allow_sign_up", Label: "允许用户注册", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "allow_org_create", Label: "允许创建组织", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "anonymous_enabled", Label: "启用匿名访问", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "anonymous_org_role", Label: "匿名访问角色", Type: "select", Required: true, Default: "Viewer", Options: []string{"Viewer", "Editor", "Admin"}, Advanced: true},
				models.DeploymentParameter{Field: "unified_alerting_enabled", Label: "启用统一告警", Type: "boolean", Required: true, Default: true, Advanced: true},
				models.DeploymentParameter{Field: "reporting_enabled", Label: "发送使用统计", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "check_for_updates", Label: "检查在线更新", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "disable_gravatar", Label: "禁用外部头像", Type: "boolean", Required: true, Default: true, Advanced: true},
				models.DeploymentParameter{Field: "cookie_secure", Label: "仅通过 HTTPS 发送会话 Cookie", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "versions_to_keep", Label: "仪表盘历史版本数", Type: "number", Required: true, Default: 20, Min: 1, Max: 100, Advanced: true},
				models.DeploymentParameter{Field: "min_refresh_interval", Label: "最小刷新周期", Type: "duration", Required: true, Default: "5s", Advanced: true},
				models.DeploymentParameter{Field: "log_mode", Label: "日志输出模式", Type: "select", Required: true, Default: "file", Options: []string{"file", "console"}, Advanced: true},
				models.DeploymentParameter{Field: "log_level", Label: "日志级别", Type: "select", Required: true, Default: "info", Options: []string{"debug", "info", "warn", "error", "critical"}, Advanced: true},
				models.DeploymentParameter{Field: "log_retention_days", Label: "文件日志保留天数", Type: "number", Required: true, Default: 7, Min: 1, Max: 365, Advanced: true},
			)
		}
		if item.component == models.ComponentNodeExporter {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "disabled_collectors", Label: "禁用默认采集器", Type: "multiselect", Default: []string{}, Options: []string{
					"arp", "bcache", "bonding", "btrfs", "boottime", "conntrack", "cpu", "cpufreq", "diskstats", "dmi", "edac", "entropy",
					"fibrechannel", "filefd", "filesystem", "hwmon", "infiniband", "ipvs", "loadavg", "mdadm", "meminfo", "netclass",
					"netdev", "netstat", "nfs", "nfsd", "nvme", "os", "powersupplyclass", "pressure", "rapl", "schedstat", "selinux",
					"sockstat", "softnet", "stat", "tapestats", "textfile", "thermal_zone", "time", "timex", "udp_queues", "uname",
					"vmstat", "watchdog", "xfs", "zfs",
				}, Help: "仅关闭明确不需要或目标系统不支持的默认采集器"},
				models.DeploymentParameter{Field: "enabled_collectors", Label: "额外启用采集器", Type: "multiselect", Default: []string{}, Options: []string{
					"buddyinfo", "cgroups", "cpu_vulnerabilities", "drm", "interrupts", "ksmd", "lnstat", "logind", "meminfo_numa",
					"mountstats", "network_route", "ntp", "perf", "processes", "qdisc", "slabinfo", "softirqs", "supervisord",
					"sysctl", "systemd", "tcpstat", "wifi", "zoneinfo",
				}, Advanced: true, Help: "额外采集器可能增加权限、内核或 systemd 依赖"},
				models.DeploymentParameter{Field: "filesystem_mount_exclude", Label: "排除挂载点正则", Type: "regex", Required: true, Default: `^/(dev|proc|run/credentials/.+|sys|var/lib/docker/.+|var/lib/containers/storage/.+)($|/)`, Advanced: true},
				models.DeploymentParameter{Field: "filesystem_type_exclude", Label: "排除文件系统类型正则", Type: "regex", Required: true, Default: `^(autofs|binfmt_misc|bpf|cgroup2?|configfs|debugfs|devpts|devtmpfs|fusectl|hugetlbfs|iso9660|mqueue|nsfs|overlay|proc|procfs|pstore|rpc_pipefs|securityfs|selinuxfs|squashfs|sysfs|tracefs)$`, Advanced: true},
				models.DeploymentParameter{Field: "disk_device_exclude", Label: "排除磁盘设备正则", Type: "regex", Advanced: true, Placeholder: `^(ram|loop|fd|(h|s|v|xvd)[a-z]|nvme\\d+n\\d+p)\\d+$`},
				models.DeploymentParameter{Field: "network_device_exclude", Label: "排除网络设备正则", Type: "regex", Advanced: true, Placeholder: `^(veth.*|docker\\d+|br-.*)$`},
				models.DeploymentParameter{Field: "web_max_requests", Label: "最大并发抓取请求", Type: "number", Required: true, Default: 40, Min: 0, Max: 10000, Advanced: true, Help: "0 表示不限制"},
				models.DeploymentParameter{Field: "disable_exporter_metrics", Label: "隐藏 Exporter 自身指标", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "runtime_gomaxprocs", Label: "Go 最大使用 CPU 数", Type: "number", Required: true, Default: 1, Min: 1, Max: 256, Advanced: true},
				models.DeploymentParameter{Field: "log_level", Label: "日志级别", Type: "select", Required: true, Default: "info", Options: []string{"debug", "info", "warn", "error"}, Advanced: true},
				models.DeploymentParameter{Field: "log_format", Label: "日志格式", Type: "select", Required: true, Default: "logfmt", Options: []string{"logfmt", "json"}, Advanced: true},
			)
		}
		if item.component == models.ComponentBlackbox {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "probe_timeout", Label: "默认探测超时", Type: "duration", Required: true, Default: "5s"},
				models.DeploymentParameter{Field: "preferred_ip_protocol", Label: "首选 IP 协议", Type: "select", Required: true, Default: "ip4", Options: []string{"ip4", "ip6"}},
				models.DeploymentParameter{Field: "http_method", Label: "HTTP 探测方法", Type: "select", Required: true, Default: "GET", Options: []string{"GET", "HEAD"}},
				models.DeploymentParameter{Field: "http_valid_status_codes", Label: "HTTP 有效状态码", Type: "multiselect", Required: true, Default: []string{"200", "301", "302"}, Options: []string{"200", "201", "202", "204", "301", "302", "303", "307", "308", "401", "403"}},
				models.DeploymentParameter{Field: "http_follow_redirects", Label: "HTTP 跟随重定向", Type: "boolean", Required: true, Default: true},
				models.DeploymentParameter{Field: "dns_query_name", Label: "DNS 默认查询名称", Type: "text", Required: true, Default: "example.com"},
				models.DeploymentParameter{Field: "dns_query_type", Label: "DNS 查询类型", Type: "select", Required: true, Default: "A", Options: []string{"A", "AAAA", "CNAME", "MX", "NS", "SRV", "TXT"}},
				models.DeploymentParameter{Field: "http_valid_versions", Label: "HTTP 协议版本", Type: "multiselect", Required: true, Default: []string{"HTTP/1.1", "HTTP/2.0"}, Options: []string{"HTTP/1.1", "HTTP/2.0"}, Advanced: true},
				models.DeploymentParameter{Field: "http_fail_if_ssl", Label: "HTTPS 探测判定失败", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "http_fail_if_not_ssl", Label: "非 HTTPS 探测判定失败", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "icmp_payload_size", Label: "ICMP 负载字节数", Type: "number", Required: true, Default: 32, Min: 0, Max: 65535, Advanced: true},
				models.DeploymentParameter{Field: "dns_valid_rcodes", Label: "DNS 有效响应码", Type: "multiselect", Required: true, Default: []string{"NOERROR"}, Options: []string{"NOERROR", "NXDOMAIN", "SERVFAIL", "REFUSED"}, Advanced: true},
				models.DeploymentParameter{Field: "history_limit", Label: "探测历史保留条数", Type: "number", Required: true, Default: 100, Min: 0, Max: 10000, Advanced: true},
				models.DeploymentParameter{Field: "external_url", Label: "外部访问地址", Type: "url", Advanced: true, Placeholder: "https://blackbox.example.com/"},
				models.DeploymentParameter{Field: "probe_log_level", Label: "探测日志级别", Type: "select", Options: []string{"debug", "info", "warn", "error"}, Advanced: true, Help: "留空使用安装版本的默认行为；仅支持 --log.prober 的版本可设置"},
				models.DeploymentParameter{Field: "log_level", Label: "服务日志级别", Type: "select", Required: true, Default: "info", Options: []string{"debug", "info", "warn", "error"}, Advanced: true},
				models.DeploymentParameter{Field: "log_format", Label: "日志格式", Type: "select", Required: true, Default: "logfmt", Options: []string{"logfmt", "json"}, Advanced: true},
			)
		}
		if item.component == models.ComponentPushgateway {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "persistence_enabled", Label: "启用指标持久化", Type: "boolean", Required: true, Default: true, Help: "写入平台管理的数据目录，重启后保留指标"},
				models.DeploymentParameter{Field: "persistence_interval", Label: "持久化写入周期", Type: "duration", Required: true, Default: "5m"},
				models.DeploymentParameter{Field: "consistency_check_enabled", Label: "启用推送一致性检查", Type: "boolean", Required: true, Default: true, Advanced: true, Help: "建议保持启用；关闭后可能接收类型或标签不一致的指标"},
				models.DeploymentParameter{Field: "enable_admin_api", Label: "启用管理清空 API", Type: "boolean", Required: true, Default: false, Advanced: true, Help: "启用后 API 可以清空全部已推送指标"},
				models.DeploymentParameter{Field: "enable_lifecycle_api", Label: "启用 HTTP 退出 API", Type: "boolean", Required: true, Default: false, Advanced: true, Help: "平台使用 systemd 管理生命周期，通常无需启用"},
				models.DeploymentParameter{Field: "external_url", Label: "外部访问地址", Type: "url", Advanced: true, Placeholder: "https://pushgateway.example.com/"},
				models.DeploymentParameter{Field: "log_level", Label: "日志级别", Type: "select", Required: true, Default: "info", Options: []string{"debug", "info", "warn", "error"}, Advanced: true},
				models.DeploymentParameter{Field: "log_format", Label: "日志格式", Type: "select", Required: true, Default: "logfmt", Options: []string{"logfmt", "json"}, Advanced: true},
			)
		}
		if item.component == models.ComponentLoki {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "retention_period", Label: "日志保留时间", Type: "duration", Required: true, Default: "720h", Help: "单节点默认保留 30 天"},
				models.DeploymentParameter{Field: "max_query_length", Label: "最大查询时间范围", Type: "duration", Required: true, Default: "720h"},
				models.DeploymentParameter{Field: "reject_old_samples", Label: "拒绝过旧日志", Type: "boolean", Required: true, Default: true},
				models.DeploymentParameter{Field: "reject_old_samples_max_age", Label: "允许补写的最大日志年龄", Type: "duration", Required: true, Default: "168h"},
				models.DeploymentParameter{Field: "ingestion_rate_mb", Label: "持续摄取速率 MiB/s", Type: "number", Required: true, Default: 16, Min: 1, Max: 10240},
				models.DeploymentParameter{Field: "ingestion_burst_size_mb", Label: "摄取突发容量 MiB", Type: "number", Required: true, Default: 24, Min: 1, Max: 10240},
				models.DeploymentParameter{Field: "query_timeout", Label: "查询超时", Type: "duration", Required: true, Default: "1m", Advanced: true},
				models.DeploymentParameter{Field: "max_query_parallelism", Label: "查询最大并行度", Type: "number", Required: true, Default: 32, Min: 1, Max: 1024, Advanced: true},
				models.DeploymentParameter{Field: "per_stream_rate_limit", Label: "单日志流持续速率", Type: "size", Required: true, Default: "3MB", Advanced: true},
				models.DeploymentParameter{Field: "per_stream_rate_limit_burst", Label: "单日志流突发容量", Type: "size", Required: true, Default: "15MB", Advanced: true},
				models.DeploymentParameter{Field: "compactor_interval", Label: "Compactor 执行周期", Type: "duration", Required: true, Default: "10m", Advanced: true},
				models.DeploymentParameter{Field: "retention_delete_delay", Label: "保留期删除延迟", Type: "duration", Required: true, Default: "2h", Advanced: true},
				models.DeploymentParameter{Field: "retention_delete_worker_count", Label: "保留期删除并发", Type: "number", Required: true, Default: 20, Min: 1, Max: 150, Advanced: true},
				models.DeploymentParameter{Field: "results_cache_max_size_mb", Label: "查询结果缓存 MiB", Type: "number", Required: true, Default: 100, Min: 1, Max: 16384, Advanced: true},
				models.DeploymentParameter{Field: "alertmanager_url", Label: "Alertmanager 地址", Type: "url", Required: true, Default: "http://127.0.0.1:9093", Advanced: true},
				models.DeploymentParameter{Field: "log_level", Label: "日志级别", Type: "select", Required: true, Default: "info", Options: []string{"debug", "info", "warn", "error"}, Advanced: true},
				models.DeploymentParameter{Field: "analytics_reporting_enabled", Label: "发送匿名使用统计", Type: "boolean", Required: true, Default: false, Advanced: true},
			)
		}
		if item.component == models.ComponentPromtail {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "client_url", Label: "Loki 推送地址", Type: "url", Required: true, Default: "http://127.0.0.1:3100/loki/api/v1/push"},
				models.DeploymentParameter{Field: "file_log_enabled", Label: "采集文件日志", Type: "boolean", Required: true, Default: true},
				models.DeploymentParameter{Field: "file_glob", Label: "日志文件路径匹配", Type: "text", Required: true, Default: "/var/log/*log"},
				models.DeploymentParameter{Field: "journal_enabled", Label: "采集 systemd Journal", Type: "boolean", Required: true, Default: true},
				models.DeploymentParameter{Field: "journal_max_age", Label: "Journal 最大回溯时间", Type: "duration", Required: true, Default: "12h"},
				models.DeploymentParameter{Field: "positions_sync_period", Label: "采集位置同步周期", Type: "duration", Required: true, Default: "10s"},
				models.DeploymentParameter{Field: "file_job_name", Label: "文件日志 Job 名称", Type: "text", Required: true, Default: "varlogs", Advanced: true},
				models.DeploymentParameter{Field: "journal_job_name", Label: "Journal Job 名称", Type: "text", Required: true, Default: "systemd-journal", Advanced: true},
				models.DeploymentParameter{Field: "journal_path", Label: "Journal 目录", Type: "text", Required: true, Default: "/var/log/journal", Advanced: true},
				models.DeploymentParameter{Field: "batch_wait", Label: "推送批次等待时间", Type: "duration", Required: true, Default: "1s", Advanced: true},
				models.DeploymentParameter{Field: "batch_size_bytes", Label: "推送批次最大字节数", Type: "number", Required: true, Default: 1048576, Min: 1024, Max: 16777216, Advanced: true},
				models.DeploymentParameter{Field: "client_timeout", Label: "Loki 请求超时", Type: "duration", Required: true, Default: "10s", Advanced: true},
				models.DeploymentParameter{Field: "backoff_min_period", Label: "重试最小等待", Type: "duration", Required: true, Default: "500ms", Advanced: true},
				models.DeploymentParameter{Field: "backoff_max_period", Label: "重试最大等待", Type: "duration", Required: true, Default: "5m", Advanced: true},
				models.DeploymentParameter{Field: "backoff_max_retries", Label: "最大重试次数", Type: "number", Required: true, Default: 10, Min: 0, Max: 100, Advanced: true},
				models.DeploymentParameter{Field: "tenant_id", Label: "租户 ID", Type: "text", Advanced: true, Placeholder: "单租户模式留空"},
				models.DeploymentParameter{Field: "drop_rate_limited_batches", Label: "丢弃限流批次", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "positions_ignore_invalid_yaml", Label: "忽略损坏的 Positions 文件", Type: "boolean", Required: true, Default: false, Advanced: true},
				models.DeploymentParameter{Field: "log_level", Label: "日志级别", Type: "select", Required: true, Default: "info", Options: []string{"debug", "info", "warn", "error"}, Advanced: true},
			)
		}
		if item.component == models.ComponentThanos {
			parameters = append(parameters,
				models.DeploymentParameter{Field: "prometheus_url", Label: "Prometheus 地址", Type: "url", Required: true, Default: "http://127.0.0.1:9090"},
				models.DeploymentParameter{Field: "prometheus_ready_timeout", Label: "等待 Prometheus 就绪超时", Type: "duration", Required: true, Default: "10m"},
				models.DeploymentParameter{Field: "prometheus_get_config_interval", Label: "Prometheus 配置刷新周期", Type: "duration", Required: true, Default: "30s"},
				models.DeploymentParameter{Field: "prometheus_get_config_timeout", Label: "Prometheus 配置请求超时", Type: "duration", Required: true, Default: "5s"},
				models.DeploymentParameter{Field: "tsdb_path", Label: "Prometheus TSDB 目录", Type: "text", Required: true, Default: "/var/lib/promlifecycle/prometheus"},
				models.DeploymentParameter{Field: "reloader_enabled", Label: "启用 Prometheus 配置重载监视", Type: "boolean", Required: true, Default: false, Advanced: true, Help: "平台已负责配置同步；仅需 Sidecar 监视规则文件时启用"},
				models.DeploymentParameter{Field: "reloader_config_file", Label: "Prometheus 配置文件", Type: "text", Required: true, Default: "/etc/prometheus/prometheus.yml", Advanced: true},
				models.DeploymentParameter{Field: "reloader_rule_dir", Label: "Prometheus 规则目录", Type: "text", Required: true, Default: "/etc/prometheus/rules", Advanced: true},
				models.DeploymentParameter{Field: "reloader_watch_interval", Label: "重载检查周期", Type: "duration", Required: true, Default: "3m", Advanced: true},
				models.DeploymentParameter{Field: "reloader_retry_interval", Label: "重载失败重试周期", Type: "duration", Required: true, Default: "5s", Advanced: true},
				models.DeploymentParameter{Field: "reloader_method", Label: "Prometheus 重载方式", Type: "select", Required: true, Default: "http", Options: []string{"http", "signal"}, Advanced: true},
				models.DeploymentParameter{Field: "http_grace_period", Label: "HTTP 优雅退出时间", Type: "duration", Required: true, Default: "2m", Advanced: true},
				models.DeploymentParameter{Field: "grpc_grace_period", Label: "gRPC 优雅退出时间", Type: "duration", Required: true, Default: "2m", Advanced: true},
				models.DeploymentParameter{Field: "grpc_max_connection_age", Label: "gRPC 最大连接时长", Type: "duration", Required: true, Default: "60m", Advanced: true},
				models.DeploymentParameter{Field: "log_level", Label: "日志级别", Type: "select", Required: true, Default: "info", Options: []string{"debug", "info", "warn", "error"}, Advanced: true},
				models.DeploymentParameter{Field: "log_format", Label: "日志格式", Type: "select", Required: true, Default: "logfmt", Options: []string{"logfmt", "json"}, Advanced: true},
			)
		}
		if item.component == models.ComponentWindowsExporter {
			parameters = []models.DeploymentParameter{
				{Field: "version", Label: "安装版本", Type: "package-version", Required: true},
				{Field: "port", Label: "Exporter 端口", Type: "number", Required: true, Default: 9182, Min: 1, Max: 65535},
				{Field: "collectors", Label: "采集器", Type: "multiselect", Required: true,
					Default: []string{"cpu", "logical_disk", "memory", "net", "os", "physical_disk", "service", "system"},
					Options: []string{"cpu", "logical_disk", "memory", "net", "os", "physical_disk", "service", "system", "iis", "mssql"}},
			}
		}
		definitions = append(definitions, models.DeploymentDefinition{
			Type: string(item.component), DisplayName: item.name, Description: item.description,
			Platform: item.platform, DeployType: item.deployType, ServiceManager: item.manager,
			HealthPath: item.healthPath, BuiltIn: true, Parameters: parameters,
		})
	}
	return definitions
}

func standardDeploymentParameters(port int, installPath, dataPath string) []models.DeploymentParameter {
	parameters := []models.DeploymentParameter{
		{Field: "version", Label: "安装版本", Type: "package-version", Required: true},
		{Field: "port", Label: "监听端口", Type: "number", Required: true, Default: port, Min: 1, Max: 65535},
		{Field: "install_path", Label: "安装路径", Type: "path", Placeholder: installPath + "-<实例ID>", Help: "留空由平台按实例 ID 分配独立目录；重新部署保留现有路径"},
	}
	if dataPath != "" {
		parameters = append(parameters, models.DeploymentParameter{Field: "data_path_override", Label: "数据路径", Type: "path", Placeholder: dataPath + "-<实例ID>", Help: "留空由平台按实例 ID 分配独立目录；重新部署保留现有路径"})
	}
	return append(parameters,
		models.DeploymentParameter{Field: "custom_args", Label: "覆盖启动参数", Type: "textarea", Advanced: true, Placeholder: "每行一个 -flag=value 或 --flag=value"},
		models.DeploymentParameter{Field: "custom_env", Label: "附加环境变量", Type: "textarea", Advanced: true, Placeholder: `JSON，例如 {"KEY":"VALUE"}`},
		models.DeploymentParameter{Field: "resource_limits", Label: "资源限制", Type: "textarea", Advanced: true, Placeholder: `{"cpu":"1","memory":"2G"}`},
	)
}

func customDefinitionParameters(definition models.ComponentDefinition) []models.DeploymentParameter {
	if strings.TrimSpace(definition.ParamSchema) != "" {
		var parameters []models.DeploymentParameter
		if json.Unmarshal([]byte(definition.ParamSchema), &parameters) == nil && len(parameters) > 0 {
			return parameters
		}
	}
	return standardDeploymentParameters(definition.DefaultPort, "/opt/promlifecycle/components/"+definition.Type, definition.DataDir)
}

func validateCustomDeploymentParameterSchema(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var parameters []models.DeploymentParameter
	if err := json.Unmarshal([]byte(raw), &parameters); err != nil {
		return fmt.Errorf("param_schema 必须是部署参数数组: %w", err)
	}
	if len(parameters) == 0 || len(parameters) > 32 {
		return fmt.Errorf("param_schema 必须包含 1 到 32 个参数")
	}
	seen := make(map[string]bool, len(parameters))
	for _, parameter := range parameters {
		if !customDeploymentFields[parameter.Field] || seen[parameter.Field] {
			return fmt.Errorf("param_schema 字段 %q 不受支持或重复", parameter.Field)
		}
		seen[parameter.Field] = true
		if expectedType := customDeploymentFieldTypes[parameter.Field]; parameter.Type != expectedType {
			return fmt.Errorf("param_schema 字段 %q 必须使用类型 %q", parameter.Field, expectedType)
		}
		if strings.TrimSpace(parameter.Label) == "" || len(parameter.Label) > 128 {
			return fmt.Errorf("param_schema 字段 %q 缺少有效 label", parameter.Field)
		}
		if parameter.Default != nil {
			switch parameter.Type {
			case "number":
				value, ok := parameter.Default.(float64)
				if !ok || value != float64(int(value)) {
					return fmt.Errorf("param_schema 字段 %q 的默认值必须是整数", parameter.Field)
				}
				if parameter.Min > 0 && int(value) < parameter.Min || parameter.Max > 0 && int(value) > parameter.Max {
					return fmt.Errorf("param_schema 字段 %q 的默认值超出范围", parameter.Field)
				}
			default:
				value, ok := parameter.Default.(string)
				if !ok {
					return fmt.Errorf("param_schema 字段 %q 的默认值必须是字符串", parameter.Field)
				}
				if parameter.Type == "path" && value != "" && (!strings.HasPrefix(value, "/") || strings.Contains(value, "..")) {
					return fmt.Errorf("param_schema 字段 %q 的默认路径无效", parameter.Field)
				}
			}
		}
		if !deploymentParameterTypes[parameter.Type] {
			return fmt.Errorf("param_schema 字段 %q 的类型 %q 不受支持", parameter.Field, parameter.Type)
		}
		if parameter.Type == "number" && parameter.Max > 0 && parameter.Min > parameter.Max {
			return fmt.Errorf("param_schema 字段 %q 的数值范围无效", parameter.Field)
		}
	}
	if !seen["version"] || !seen["port"] || !seen["install_path"] {
		return fmt.Errorf("param_schema 必须包含 version、port 和 install_path")
	}
	return nil
}

func normalizeCustomDeploymentParameterSchema(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		if err := validateCustomDeploymentParameterSchema(value); err != nil {
			return "", err
		}
		return value, nil
	}
	var parameters []models.DeploymentParameter
	if err := json.Unmarshal(raw, &parameters); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return "", err
	}
	if err := validateCustomDeploymentParameterSchema(string(encoded)); err != nil {
		return "", err
	}
	return string(encoded), nil
}
