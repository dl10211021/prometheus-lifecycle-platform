package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

const onboardingEvidenceFreshness = 24 * time.Hour

var (
	ErrDiscoveredNotFound       = errors.New("发现项已变化或不存在，请重新扫描")
	ErrDiscoveredManaged        = errors.New("发现项已被平台纳管，请从组件生命周期删除")
	ErrDiscoveredSharedArtifact = errors.New("发现项使用共享安装产物")
)

type OnboardingService struct {
	db       *gorm.DB
	basePath string
	client   *http.Client
}

type PreflightCheck struct {
	Key         string      `json:"key"`
	Name        string      `json:"name"`
	Status      string      `json:"status"`
	Reason      string      `json:"reason"`
	Value       interface{} `json:"value,omitempty"`
	Recommended string      `json:"recommended,omitempty"`
	Blocking    bool        `json:"blocking"`
}

type PreflightReport struct {
	HostID    uint             `json:"host_id"`
	CheckedAt time.Time        `json:"checked_at"`
	Checks    []PreflightCheck `json:"checks"`
	Blockers  []string         `json:"blockers"`
	Ready     bool             `json:"ready"`
	Summary   map[string]int   `json:"summary"`
}

type DiscoveryPermissions struct {
	ConfigReadable  bool `json:"config_readable"`
	ConfigWritable  bool `json:"config_writable"`
	DataReadable    bool `json:"data_readable"`
	DataWritable    bool `json:"data_writable"`
	ServiceWritable bool `json:"service_writable"`
}

type DiscoveredComponent struct {
	OwnershipConflict bool                 `json:"ownership_conflict,omitempty"`
	Type              string               `json:"type"`
	Name              string               `json:"name"`
	Port              int                  `json:"port"`
	PID               int                  `json:"pid,omitempty"`
	Unit              string               `json:"unit,omitempty"`
	Runtime           string               `json:"runtime,omitempty"`
	Binary            string               `json:"bin,omitempty"`
	Config            string               `json:"config,omitempty"`
	Data              string               `json:"data,omitempty"`
	Version           string               `json:"version,omitempty"`
	Health            string               `json:"health"`
	HealthReason      string               `json:"health_reason"`
	Permissions       DiscoveryPermissions `json:"permissions"`
	Confidence        float64              `json:"confidence"`
	ConfidenceTag     string               `json:"confidence_label"`
	Fingerprint       string               `json:"fingerprint"`
	Evidence          []string             `json:"evidence"`
	AlreadyOwned      string               `json:"ownership,omitempty"`
	ComponentID       uint                 `json:"component_id,omitempty"`
}

type DiscoveryDeletionResult struct {
	Deleted   DiscoveredComponent   `json:"deleted"`
	Removed   []string              `json:"removed"`
	Preserved []string              `json:"preserved"`
	Remaining []DiscoveredComponent `json:"remaining"`
	Verified  map[string]bool       `json:"verified"`
}

type AcceptanceCheck struct {
	Key       string      `json:"key"`
	Status    string      `json:"status"`
	Reason    string      `json:"reason"`
	Evidence  interface{} `json:"evidence,omitempty"`
	CheckedAt time.Time   `json:"checked_at"`
}

type AcceptanceReport struct {
	CheckedAt time.Time         `json:"checked_at"`
	Passed    bool              `json:"passed"`
	Checks    []AcceptanceCheck `json:"checks"`
}

type OnboardingStep struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Completed bool   `json:"completed"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Blocking  bool   `json:"blocking"`
	Action    string `json:"action"`
}

type OnboardingStatus struct {
	Protected       bool             `json:"protected"`
	Completed       bool             `json:"completed"`
	CompletedSteps  int              `json:"completed_steps"`
	TotalSteps      int              `json:"total_steps"`
	Steps           []OnboardingStep `json:"steps"`
	LastEvaluatedAt time.Time        `json:"last_evaluated_at"`
}

type StackResourceRecommendation struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Disk   string `json:"disk,omitempty"`
}

// StackVersionProfile pins a jointly exercised combination, not independently
// selected latest releases. Package availability remains a live inventory check.
type StackVersionProfile struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Platform    string            `json:"platform"`
	ValidatedAt string            `json:"validated_at"`
	Scope       string            `json:"scope"`
	Versions    map[string]string `json:"versions"`
	Evidence    []string          `json:"evidence"`
}

type StackTemplate struct {
	Key                  string                                 `json:"key"`
	Name                 string                                 `json:"name"`
	Description          string                                 `json:"description"`
	ComponentOrder       []string                               `json:"component_order"`
	RecommendedResources map[string]StackResourceRecommendation `json:"recommended_resources"`
	Dependencies         map[string][]string                    `json:"dependencies"`
	AcceptanceConditions []string                               `json:"acceptance_conditions"`
	VersionProfile       *StackVersionProfile                   `json:"version_profile,omitempty"`
}

func NewOnboardingService(db *gorm.DB, basePath string) *OnboardingService {
	if strings.TrimSpace(basePath) == "" {
		basePath = "."
	}
	return &OnboardingService{
		db: db, basePath: basePath,
		client: &http.Client{Timeout: 3 * time.Second},
	}
}

// EnsureLocalHost makes first-run onboarding independent of manual host entry.
// It only owns fields that describe the platform-local execution channel and
// preserves user-authored role/description values on an existing row.
func EnsureLocalHost(db *gorm.DB, basePath string) (*models.Host, error) {
	if db == nil {
		return nil, fmt.Errorf("必须配置数据库")
	}
	osName, osVersion := localOS()
	status := "online"
	now := time.Now()
	var host models.Host
	err := db.Where("ip = ?", "127.0.0.1").First(&host).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	if err == gorm.ErrRecordNotFound {
		host = models.Host{Name: "本机", IP: "127.0.0.1", Role: "monitor", Description: "平台自动发现的本机节点"}
	}
	host.OS = osName
	host.OSVersion = osVersion
	host.ConnectionType = "local"
	host.Port = 0
	host.Username = ""
	host.Status = status
	host.IsWindows = runtime.GOOS == "windows"
	host.HasDocker = false
	host.DockerVersion = ""
	host.LastOnline = &now
	if host.Name == "" {
		host.Name = "本机"
	}
	if err == gorm.ErrRecordNotFound {
		if createErr := db.Create(&host).Error; createErr != nil {
			// A concurrent startup may have created the IP. If instead only the
			// friendly name conflicts, retry with a deterministic local name.
			if loadErr := db.Where("ip = ?", "127.0.0.1").First(&host).Error; loadErr != nil {
				host.ID = 0
				host.Name = "本机 127.0.0.1"
				if retryErr := db.Create(&host).Error; retryErr != nil {
					if finalLoadErr := db.Where("ip = ?", "127.0.0.1").First(&host).Error; finalLoadErr != nil {
						return nil, retryErr
					}
				}
			}
		}
	} else if saveErr := db.Save(&host).Error; saveErr != nil {
		return nil, saveErr
	}
	return &host, nil
}

func (s *OnboardingService) Preflight() (PreflightReport, error) {
	host, err := EnsureLocalHost(s.db, s.basePath)
	if err != nil {
		return PreflightReport{}, err
	}
	report := PreflightReport{HostID: host.ID, CheckedAt: time.Now(), Checks: make([]PreflightCheck, 0, 10), Summary: map[string]int{"pass": 0, "warn": 0, "fail": 0}}
	add := func(check PreflightCheck) {
		report.Checks = append(report.Checks, check)
		report.Summary[check.Status]++
		if check.Blocking && check.Status == "fail" {
			report.Blockers = append(report.Blockers, check.Key)
		}
	}

	osName, osVersion := localOS()
	osCheck := PreflightCheck{Key: "os", Name: "操作系统", Status: "pass", Reason: "已识别受支持的 Linux 环境", Value: osVersion, Recommended: "Linux x86_64/arm64", Blocking: true}
	if osName != "linux" {
		osCheck.Status, osCheck.Reason = "fail", "平台托管部署需要 Linux 本机"
	}
	add(osCheck)

	systemdState, systemdOK := commandOutput(2*time.Second, "systemctl", "is-system-running")
	runtimeStatus := "pass"
	runtimeReason := "systemd 可用于二进制安装和开机自启动"
	if !systemdOK && strings.TrimSpace(systemdState) != "degraded" {
		runtimeStatus, runtimeReason = "fail", "systemd 不可用，无法安装和托管组件"
	}
	add(PreflightCheck{Key: "runtime", Name: "systemd", Status: runtimeStatus, Reason: runtimeReason, Value: strings.TrimSpace(systemdState), Recommended: "安装并启用 systemd", Blocking: true})

	cpus := runtime.NumCPU()
	cpuStatus, cpuReason := "pass", "CPU 满足基础监控栈建议"
	if cpus < 2 {
		cpuStatus, cpuReason = "warn", "CPU 少于 2 核，采集和查询并发会受限"
	}
	add(PreflightCheck{Key: "cpu", Name: "CPU", Status: cpuStatus, Reason: cpuReason, Value: cpus, Recommended: "至少 2 核，完整监控建议 4 核", Blocking: false})

	memoryBytes := localMemoryBytes()
	memoryStatus, memoryReason := "pass", "内存满足基础监控栈建议"
	if memoryBytes < 2<<30 {
		memoryStatus, memoryReason = "fail", "可用物理内存少于 2 GiB"
	} else if memoryBytes < 4<<30 {
		memoryStatus, memoryReason = "warn", "内存可运行基础栈，完整监控建议扩容"
	}
	add(PreflightCheck{Key: "memory", Name: "内存", Status: memoryStatus, Reason: memoryReason, Value: memoryBytes, Recommended: "基础指标至少 2 GiB，完整监控至少 4 GiB", Blocking: true})

	diskBytes, diskErr := availableDiskBytes(s.basePath)
	diskStatus, diskReason := "pass", "数据盘可用空间满足最低要求"
	if diskErr != nil {
		diskStatus, diskReason = "fail", "无法读取数据盘容量: "+diskErr.Error()
	} else if diskBytes < 5<<30 {
		diskStatus, diskReason = "fail", "数据盘可用空间少于 5 GiB"
	} else if diskBytes < 20<<30 {
		diskStatus, diskReason = "warn", "空间可用于试用，生产保留期可能不足"
	}
	add(PreflightCheck{Key: "disk", Name: "磁盘", Status: diskStatus, Reason: diskReason, Value: diskBytes, Recommended: "至少 20 GiB 可用空间", Blocking: true})

	writeStatus, writeReason := "pass", "平台数据目录可写"
	if err := probeWritable(s.basePath); err != nil {
		writeStatus, writeReason = "fail", "平台数据目录不可写: "+err.Error()
	}
	add(PreflightCheck{Key: "write_permission", Name: "写权限", Status: writeStatus, Reason: writeReason, Value: s.basePath, Recommended: "运行用户可写平台目录、配置目录与数据目录", Blocking: true})

	ntp, ntpOK := commandOutput(2*time.Second, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	timeStatus, timeReason := "pass", "系统时间已同步"
	if !ntpOK || strings.TrimSpace(strings.ToLower(ntp)) != "yes" {
		timeStatus, timeReason = "warn", "未确认 NTP 同步，告警时间线可能产生偏差"
	}
	add(PreflightCheck{Key: "time_sync", Name: "时间同步", Status: timeStatus, Reason: timeReason, Value: strings.TrimSpace(ntp), Recommended: "启用 chrony 或 systemd-timesyncd", Blocking: false})

	portDetails := map[string]string{}
	conflicted := 0
	for _, port := range []int{3000, 3100, 9080, 9090, 9091, 9093, 9100, 9115} {
		if portAvailable(port) {
			portDetails[strconv.Itoa(port)] = "available"
		} else {
			portDetails[strconv.Itoa(port)] = "in_use"
			conflicted++
		}
	}
	portStatus, portReason := "pass", "常用组件端口可用"
	if conflicted > 0 {
		portStatus, portReason = "warn", fmt.Sprintf("%d 个常用端口已被占用，请先扫描并选择观察或接管", conflicted)
	}
	add(PreflightCheck{Key: "ports", Name: "端口", Status: portStatus, Reason: portReason, Value: portDetails, Recommended: "3000/3100/9080/9090/9091/9093/9100/9115 可用，或接管已存在组件", Blocking: false})

	packageCount, packageTypes := s.availableOfflinePackages()
	pkgStatus, pkgReason := "pass", fmt.Sprintf("检测到 %d 个可用离线包", packageCount)
	if packageCount == 0 {
		pkgStatus, pkgReason = "warn", "未检测到可用离线包，离线环境部署前需先上传"
	}
	add(PreflightCheck{Key: "offline_packages", Name: "离线包", Status: pkgStatus, Reason: pkgReason, Value: packageTypes, Recommended: "至少准备 Prometheus 与 node_exporter 安装包", Blocking: false})

	report.Ready = len(report.Blockers) == 0
	return report, nil
}

func (s *OnboardingService) DiscoverLocal() ([]DiscoveredComponent, error) {
	host, err := EnsureLocalHost(s.db, s.basePath)
	if err != nil {
		return nil, err
	}
	listeners := localListeners()
	result := make([]DiscoveredComponent, 0)
	for _, spec := range discoverySpecs() {
		for _, item := range s.discoverComponents(spec, listeners) {
			var component models.Component
			if err := s.db.Where("host_id = ? AND type = ? AND port = ? AND status <> ?", host.ID, spec.kind, item.Port, "removed").First(&component).Error; err == nil {
				item.ComponentID = component.ID
				var adoption models.ComponentAdoption
				if s.db.Where("component_id = ?", component.ID).First(&adoption).Error == nil {
					item.AlreadyOwned = adoption.Ownership
				} else {
					item.AlreadyOwned = "platform"
				}
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Confidence != result[j].Confidence {
			return result[i].Confidence > result[j].Confidence
		}
		if result[i].Type != result[j].Type {
			return result[i].Type < result[j].Type
		}
		if result[i].Port != result[j].Port {
			return result[i].Port < result[j].Port
		}
		return result[i].Binary < result[j].Binary
	})
	return result, nil
}

func (s *OnboardingService) DeleteDiscovered(ctx context.Context, hostID uint, fingerprint string) (*DiscoveryDeletionResult, error) {
	localHost, err := EnsureLocalHost(s.db, s.basePath)
	if err != nil {
		return nil, err
	}
	if hostID != localHost.ID || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(fingerprint) {
		return nil, ErrDiscoveredNotFound
	}
	items, err := s.DiscoverLocal()
	if err != nil {
		return nil, err
	}
	var item *DiscoveredComponent
	for index := range items {
		if items[index].Fingerprint == fingerprint {
			item = &items[index]
			break
		}
	}
	if item == nil {
		return nil, ErrDiscoveredNotFound
	}
	if item.ComponentID != 0 || item.AlreadyOwned != "" {
		return nil, ErrDiscoveredManaged
	}
	if err := s.guardDiscoveredArtifacts(ctx, *localHost, *item, items); err != nil {
		return nil, err
	}

	result := &DiscoveryDeletionResult{Deleted: *item, Removed: []string{}, Preserved: []string{}, Verified: map[string]bool{}}
	if item.Unit != "" {
		if err := stopDiscoveredUnit(ctx, item.Unit, &result.Removed); err != nil {
			return nil, err
		}
	}
	if item.PID > 0 {
		if err := stopDiscoveredProcess(item.PID, item.Binary); err != nil {
			return nil, err
		}
		result.Removed = append(result.Removed, fmt.Sprintf("pid:%d", item.PID))
	}
	if err := uninstallDiscoveredBinary(ctx, *item, &result.Removed); err != nil {
		return nil, err
	}
	if err := removeRemainingDiscoveredUnit(ctx, item.Unit, &result.Removed); err != nil {
		return nil, err
	}
	if item.Config != "" {
		result.Preserved = append(result.Preserved, item.Config)
	}
	if item.Data != "" {
		result.Preserved = append(result.Preserved, item.Data)
	}
	time.Sleep(250 * time.Millisecond)
	if err := verifyDiscoveredRemoval(*item, result.Verified); err != nil {
		return nil, err
	}
	result.Remaining, err = s.DiscoverLocal()
	if err != nil {
		return nil, err
	}
	return result, nil
}

func sameDiscoveryArtifact(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	if a == b {
		return true
	}
	left, leftErr := os.Stat(a)
	right, rightErr := os.Stat(b)
	return leftErr == nil && rightErr == nil && os.SameFile(left, right)
}

func sharedDiscoveryReference(item DiscoveredComponent, others []DiscoveredComponent) error {
	for _, other := range others {
		if other.Fingerprint == item.Fingerprint {
			continue
		}
		if sameDiscoveryArtifact(item.Binary, other.Binary) ||
			(item.Unit != "" && deployer.CanonicalServiceName(item.Unit) == deployer.CanonicalServiceName(other.Unit)) {
			return fmt.Errorf("%w: %s is referenced by %s (unit=%s PID=%d component=%d); preserve it and migrate that instance to a separate executable before retrying",
				ErrDiscoveredSharedArtifact, item.Binary, other.Name, other.Unit, other.PID, other.ComponentID)
		}
	}
	return nil
}

func (s *OnboardingService) guardDiscoveredArtifacts(ctx context.Context, host models.Host, item DiscoveredComponent, items []DiscoveredComponent) error {
	if err := sharedDiscoveryReference(item, items); err != nil {
		return err
	}
	var managed []models.Component
	if err := s.db.Where("host_id = ? AND status <> ?", host.ID, "removed").Find(&managed).Error; err != nil {
		return err
	}
	for _, component := range managed {
		path := component.BinaryPath
		if path == "" && component.InstallPath != "" {
			path = filepath.Join(component.InstallPath, string(component.Type))
			if component.Type == models.ComponentGrafana {
				path = filepath.Join(component.InstallPath, "bin", "grafana")
			}
		}
		if sameDiscoveryArtifact(path, item.Binary) ||
			(item.Unit != "" && deployer.CanonicalServiceName(component.ServiceName) == deployer.CanonicalServiceName(item.Unit)) {
			return fmt.Errorf("%w: executable %s is retained by managed component %d (%s); use separate executables before deleting this discovery", ErrDiscoveredSharedArtifact, item.Binary, component.ID, component.Name)
		}
		if path != "" {
			items = append(items, DiscoveredComponent{Binary: path, ComponentID: component.ID})
		}
	}
	owner, err := discoveredPackageOwner(item.Binary)
	if err != nil {
		return err
	}
	paths := []string{item.Binary}
	if owner != nil {
		for _, other := range items {
			if other.Fingerprint == item.Fingerprint || other.Binary == "" {
				continue
			}
			otherOwner, err := discoveredPackageOwner(other.Binary)
			if err != nil {
				return err
			}
			if otherOwner != nil && *otherOwner == *owner {
				return fmt.Errorf("%w: package %s also supplies %s (component %d); split package ownership before uninstalling", ErrDiscoveredSharedArtifact, owner.Name, other.Binary, other.ComponentID)
			}
		}
		var output []byte
		if owner.Manager == "rpm" {
			output, err = exec.CommandContext(ctx, "rpm", "-ql", owner.Name).CombinedOutput()
		} else {
			output, err = exec.CommandContext(ctx, "dpkg-query", "-L", owner.Name).CombinedOutput()
		}
		if err != nil {
			return fmt.Errorf("cannot enumerate package %s before removal: %w", owner.Name, err)
		}
		for _, path := range strings.Split(string(output), "\n") {
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if info.Mode()&0111 != 0 {
				paths = append(paths, path)
			}
			if strings.HasSuffix(path, ".service") && filepath.Base(path) != deployer.CanonicalServiceName(item.Unit)+".service" {
				return fmt.Errorf("%w: package %s owns additional unit %s; remove its other instances explicitly before retrying", ErrDiscoveredSharedArtifact, owner.Name, path)
			}
		}
	}
	if err := deployer.GuardHostArtifactReferences(ctx, host, item.Unit, item.PID, paths); err != nil {
		return fmt.Errorf("%w: %v", ErrDiscoveredSharedArtifact, err)
	}
	return nil
}

func (s *OnboardingService) Acceptance(channelID uint, sendNotification bool) (AcceptanceReport, error) {
	now := time.Now()
	report := AcceptanceReport{CheckedAt: now, Checks: make([]AcceptanceCheck, 0, 4)}
	prometheus, promErr := s.runningComponent(models.ComponentPrometheus)
	if promErr != nil {
		for _, key := range []string{"prometheus", "targets", "rules"} {
			report.Checks = append(report.Checks, AcceptanceCheck{Key: key, Status: "fail", Reason: "未找到可实时访问的 Prometheus", CheckedAt: now})
		}
	} else {
		baseURL := componentURL(prometheus, 9090)
		report.Checks = append(report.Checks, s.checkPrometheusHealth(baseURL, now))
		report.Checks = append(report.Checks, s.checkPrometheusTargets(baseURL, now))
		report.Checks = append(report.Checks, s.checkPrometheusRules(baseURL, now))
	}

	if sendNotification {
		report.Checks = append(report.Checks, s.sendAcceptanceNotification(channelID, now))
	} else {
		report.Checks = append(report.Checks, s.recentNotificationEvidence(now))
	}
	if sendNotification {
		report.Checks = append(report.Checks, s.checkFreshBackup(now))
	} else {
		report.Checks = append(report.Checks, s.recentBackupEvidence(now))
	}
	report.Passed = true
	for _, check := range report.Checks {
		if check.Status != "pass" {
			report.Passed = false
		}
		if sendNotification {
			s.recordCheck(check)
		}
	}
	return report, nil
}

func (s *OnboardingService) Status() (OnboardingStatus, error) {
	preflight, err := s.Preflight()
	if err != nil {
		return OnboardingStatus{}, err
	}
	acceptance, err := s.Acceptance(0, false)
	if err != nil {
		return OnboardingStatus{}, err
	}
	checks := map[string]AcceptanceCheck{}
	for _, check := range acceptance.Checks {
		checks[check.Key] = check
	}
	prom := checks["prometheus"]
	target := checks["targets"]
	rules := checks["rules"]
	notify := checks["notifications"]
	backup := checks["backup"]
	steps := []OnboardingStep{
		{Key: "host_preflight", Name: "本机预检", Completed: preflight.Ready, Status: passFail(preflight.Ready), Reason: preflightReason(preflight), Blocking: true, Action: "/onboarding/preflight"},
		{Key: "prometheus_healthy", Name: "Prometheus 健康", Completed: prom.Status == "pass", Status: prom.Status, Reason: prom.Reason, Blocking: true, Action: "/components"},
		{Key: "target_up", Name: "采集目标在线", Completed: target.Status == "pass", Status: target.Status, Reason: target.Reason, Blocking: true, Action: "/scrape-configs"},
		{Key: "rules_notifications", Name: "规则与通知测试", Completed: rules.Status == "pass" && notify.Status == "pass", Status: combinedStatus(rules, notify), Reason: rules.Reason + "；" + notify.Reason, Blocking: true, Action: "/onboarding/acceptance"},
		{Key: "verified_backup", Name: "已验证的新鲜备份", Completed: backup.Status == "pass", Status: backup.Status, Reason: backup.Reason, Blocking: true, Action: "/backups"},
	}
	completed := 0
	for _, step := range steps {
		if step.Completed {
			completed++
		}
	}
	var protectedUsers int64
	if s.db != nil {
		s.db.Model(&models.User{}).Where("is_active = ? AND must_change_password = ? AND role IN ?", true, false, []string{"system_admin", "change_admin"}).Count(&protectedUsers)
	}
	return OnboardingStatus{Protected: protectedUsers > 0, Completed: completed == len(steps), CompletedSteps: completed, TotalSteps: len(steps), Steps: steps, LastEvaluatedAt: time.Now()}, nil
}

func StackTemplates() []StackTemplate {
	return []StackTemplate{
		{
			Key: "basic_metrics", Name: "基础指标", Description: "单机与基础设施指标采集、查询和可视化",
			ComponentOrder: []string{"node_exporter", "prometheus", "grafana"},
			RecommendedResources: map[string]StackResourceRecommendation{
				"node_exporter": {CPU: "0.1 core", Memory: "64 MiB"},
				"prometheus":    {CPU: "1 core", Memory: "2 GiB", Disk: "20 GiB+"},
				"grafana":       {CPU: "0.5 core", Memory: "512 MiB", Disk: "1 GiB"},
			},
			Dependencies:         map[string][]string{"prometheus": {"node_exporter"}, "grafana": {"prometheus"}},
			AcceptanceConditions: []string{"Prometheus /-/healthy 返回成功", "至少一个 active target 实时状态为 up", "Grafana 数据源连接测试成功"},
		},
		{
			Key: "batch_metrics", Name: "批任务指标", Description: "Pushgateway 持久化接收短任务指标，并由 Prometheus 采集和 Grafana 展示",
			ComponentOrder: []string{"pushgateway", "prometheus", "grafana"},
			RecommendedResources: map[string]StackResourceRecommendation{
				"pushgateway": {CPU: "0.25 core", Memory: "256 MiB", Disk: "1 GiB+"},
				"prometheus":  {CPU: "1 core", Memory: "2 GiB", Disk: "20 GiB+"},
				"grafana":     {CPU: "0.5 core", Memory: "512 MiB", Disk: "1 GiB"},
			},
			Dependencies:         map[string][]string{"prometheus": {"pushgateway"}, "grafana": {"prometheus"}},
			AcceptanceConditions: []string{"Pushgateway /-/ready 返回成功", "推送测试指标后 Prometheus target 为 up", "重启后已推送指标仍存在", "Grafana 数据源连接测试成功"},
		},
		{
			Key: "thanos_sidecar", Name: "Thanos Sidecar", Description: "为单节点 Prometheus 提供 StoreAPI、元数据和配置重载能力",
			ComponentOrder: []string{"prometheus", "thanos"},
			RecommendedResources: map[string]StackResourceRecommendation{
				"prometheus": {CPU: "1 core", Memory: "2 GiB", Disk: "20 GiB+"},
				"thanos":     {CPU: "0.5 core", Memory: "512 MiB"},
			},
			Dependencies:         map[string][]string{"thanos": {"prometheus"}},
			AcceptanceConditions: []string{"Prometheus 包含唯一 external_labels", "Thanos Sidecar /-/ready 返回成功", "StoreAPI gRPC 端口 10901 可访问", "Sidecar 能读取 Prometheus 元数据"},
		},
		{
			Key: "windows_metrics", Name: "Windows 主机指标", Description: "通过 WinRM 安装并管理 Windows Exporter MSI",
			ComponentOrder: []string{"windows_exporter"},
			RecommendedResources: map[string]StackResourceRecommendation{
				"windows_exporter": {CPU: "0.2 core", Memory: "128 MiB"},
			},
			Dependencies:         map[string][]string{},
			AcceptanceConditions: []string{"目标主机使用 Windows WinRM 连接", "Windows Exporter 服务状态为 Running", "9182 /metrics 返回成功", "Prometheus 自动生成 Windows 抓取目标"},
		},
		{
			Key: "full_monitoring", Name: "完整监控", Description: "指标、探测、规则、告警路由和可视化的完整闭环",
			ComponentOrder: []string{"node_exporter", "blackbox_exporter", "prometheus", "alertmanager", "grafana"},
			RecommendedResources: map[string]StackResourceRecommendation{
				"node_exporter":     {CPU: "0.1 core", Memory: "64 MiB"},
				"blackbox_exporter": {CPU: "0.25 core", Memory: "128 MiB"},
				"prometheus":        {CPU: "2 cores", Memory: "4 GiB", Disk: "50 GiB+"},
				"alertmanager":      {CPU: "0.25 core", Memory: "256 MiB", Disk: "1 GiB"},
				"grafana":           {CPU: "0.5 core", Memory: "1 GiB", Disk: "2 GiB"},
			},
			Dependencies:         map[string][]string{"prometheus": {"node_exporter", "blackbox_exporter"}, "alertmanager": {"prometheus"}, "grafana": {"prometheus"}},
			AcceptanceConditions: []string{"Prometheus 与 Alertmanager 健康检查成功", "至少一个采集目标实时为 up", "规则 API 返回健康规则", "测试通知实际发送成功", "新鲜备份可读取并通过校验"},
			VersionProfile: &StackVersionProfile{
				ID: "linux-single-node-20260909", Name: "完整监控 · 安全升级验收组合",
				Platform: "Linux amd64 / binary + systemd / RHEL 8 glibc 2.28", ValidatedAt: "2026-09-09",
				Scope: "单节点指标、远程主机采集、黑盒探测、告警通知与 Grafana；Prometheus 与 Grafana 已按当日官方安全公告复核，不是未来零漏洞保证。不包含 Windows、日志栈或 Thanos；变更任一版本后需重新验收。",
				Versions: map[string]string{
					"prometheus": "3.13.3", "alertmanager": "0.27.0", "grafana": "13.1.5",
					"node_exporter": "1.8.2", "blackbox_exporter": "0.26.0",
				},
				Evidence: []string{
					"官方归档 SHA256 核验；两个安全升级版本在 RHEL 8 实际运行",
					"Prometheus 历史样本在升级与恢复匹配旧数据后仍可查询",
					"Grafana 现网 SQLite 副本迁移；原用户、仪表盘及加密数据源可用",
					"远程 Blackbox 安装、升级、回退及 Alertmanager 配置恢复通过",
					"受控 SMTP 实际收信、拒收和重试；不代表组织 SMTP/TLS 已验收",
				},
			},
		},
		{
			Key: "logs_monitoring", Name: "日志监控", Description: "主机日志采集、集中存储和 Grafana 检索",
			ComponentOrder: []string{"loki", "promtail", "grafana"},
			RecommendedResources: map[string]StackResourceRecommendation{
				"loki":     {CPU: "2 cores", Memory: "4 GiB", Disk: "100 GiB+（按保留期调整）"},
				"promtail": {CPU: "0.25 core", Memory: "256 MiB"},
				"grafana":  {CPU: "0.5 core", Memory: "1 GiB", Disk: "2 GiB"},
			},
			Dependencies:         map[string][]string{"promtail": {"loki"}, "grafana": {"loki"}},
			AcceptanceConditions: []string{"Loki /ready 返回成功", "Promtail /ready 返回成功", "Loki 接收到新日志流", "Grafana Loki 数据源连接测试成功"},
		},
	}
}

type componentDiscoverySpec struct {
	kind, name string
	port       int
	units      []string
	bins       []string
	configs    []string
	data       []string
	healthPath string
}

type systemdDiscovery struct {
	Unit, Binary string
	PID, Port    int
}

func discoverySpecs() []componentDiscoverySpec {
	return []componentDiscoverySpec{
		{"prometheus", "Prometheus", 9090, []string{"prometheus.service"}, []string{"prometheus", "/usr/local/bin/prometheus", "/usr/bin/prometheus"}, []string{"/etc/prometheus/prometheus.yml"}, []string{"/var/lib/prometheus"}, "/-/healthy"},
		{"alertmanager", "Alertmanager", 9093, []string{"alertmanager.service"}, []string{"alertmanager", "/usr/local/bin/alertmanager", "/usr/bin/alertmanager"}, []string{"/etc/alertmanager/alertmanager.yml"}, []string{"/var/lib/alertmanager"}, "/-/healthy"},
		{"grafana", "Grafana", 3000, []string{"grafana-server.service", "grafana.service"}, []string{"grafana-server", "/usr/sbin/grafana-server"}, []string{"/etc/grafana/grafana.ini"}, []string{"/var/lib/grafana"}, "/api/health"},
		{"node_exporter", "Node Exporter", 9100, []string{"node_exporter.service"}, []string{"node_exporter", "/usr/local/bin/node_exporter", "/usr/bin/node_exporter"}, nil, nil, "/metrics"},
		{"blackbox_exporter", "Blackbox Exporter", 9115, []string{"blackbox_exporter.service"}, []string{"blackbox_exporter", "/usr/local/bin/blackbox_exporter"}, []string{"/etc/blackbox_exporter/config.yml"}, nil, "/-/healthy"},
		{"pushgateway", "Pushgateway", 9091, []string{"pushgateway.service"}, []string{"pushgateway", "/usr/local/bin/pushgateway"}, nil, []string{"/var/lib/pushgateway"}, "/-/healthy"},
		{"loki", "Loki", 3100, []string{"loki.service"}, []string{"loki", "/usr/local/bin/loki"}, []string{"/etc/loki/config.yaml", "/etc/loki/loki.yaml"}, []string{"/var/lib/loki"}, "/ready"},
		{"promtail", "Promtail", 9080, []string{"promtail.service"}, []string{"promtail", "/usr/local/bin/promtail"}, []string{"/etc/promtail/config.yaml", "/etc/promtail/promtail.yaml"}, nil, "/ready"},
		{"thanos", "Thanos Sidecar", 10902, []string{"thanos.service"}, []string{"thanos", "/usr/local/bin/thanos"}, nil, nil, "/-/ready"},
	}
}

func (s *OnboardingService) discoverComponents(spec componentDiscoverySpec, listeners map[int]int) []DiscoveredComponent {
	items := make([]DiscoveredComponent, 0)
	seenPIDs := map[int]bool{}

	processPorts := map[int][]int{}
	for port, pid := range listeners {
		if pid <= 0 || !processMatchesDiscoverySpec(pid, spec) {
			continue
		}
		processPorts[pid] = append(processPorts[pid], port)
	}
	pids := make([]int, 0, len(processPorts))
	for pid := range processPorts {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		port := preferredDiscoveryPort(processPorts[pid], spec.port)
		items = append(items, s.discoverComponentAt(spec, port, pid, "", ""))
		seenPIDs[pid] = true
	}

	for _, unit := range discoverySystemdUnits(spec) {
		if unit.PID > 0 && seenPIDs[unit.PID] {
			continue
		}
		port := unit.Port
		if ports := processPorts[unit.PID]; len(ports) > 0 {
			port = preferredDiscoveryPort(ports, spec.port)
		}
		items = append(items, s.discoverComponentAt(spec, port, unit.PID, unit.Unit, unit.Binary))
		if unit.PID > 0 {
			seenPIDs[unit.PID] = true
		}
	}
	return items
}

func (s *OnboardingService) discoverComponentAt(spec componentDiscoverySpec, port, pid int, unit, binary string) DiscoveredComponent {
	item := DiscoveredComponent{Type: spec.kind, Name: spec.name, Port: port, PID: pid, Unit: unit, Binary: binary, Health: "unknown", Permissions: DiscoveryPermissions{}, Evidence: []string{}}
	if pid > 0 {
		item.Runtime = "process"
		item.Evidence = append(item.Evidence, fmt.Sprintf("port %d pid %d", port, pid))
		item.Confidence += .45
		if exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
			item.Binary = strings.TrimSuffix(exePath, " (deleted)")
		}
		if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			args := strings.Split(string(cmdline), "\x00")
			item.Config = findArgument(args, "--config.file", "-config.file", "--config")
			item.Data = findArgument(args, "--storage.tsdb.path", "--storage.path", "--data.path", "--data-path")
		}
		if item.Unit == "" {
			item.Unit = unitForPID(pid)
		}
	}
	unitActive := false
	if item.Unit != "" {
		state, _ := commandOutput(time.Second, "systemctl", "is-active", item.Unit)
		enabled, _ := commandOutput(time.Second, "systemctl", "is-enabled", item.Unit)
		unitActive = strings.TrimSpace(state) == "active"
		item.Runtime = "systemd"
		item.Evidence = append(item.Evidence, "systemd unit "+item.Unit+" state "+strings.TrimSpace(state)+" / "+strings.TrimSpace(enabled))
		item.Confidence += .2
	}
	if item.Config == "" {
		item.Config = firstExisting(spec.configs)
	}
	if item.Data == "" {
		item.Data = firstExisting(spec.data)
	}
	if item.Binary != "" {
		item.Evidence = append(item.Evidence, "binary "+item.Binary)
		item.Confidence += .15
		item.Version = binaryVersion(item.Binary)
	}
	if item.Config != "" {
		item.Evidence = append(item.Evidence, "config "+item.Config)
		item.Confidence += .1
	}
	if item.Data != "" {
		item.Evidence = append(item.Evidence, "data "+item.Data)
	}
	if item.Unit != "" && item.PID == 0 && !unitActive {
		item.Health, item.HealthReason = "stopped", "systemd 单元已安装但当前未运行"
	} else {
		healthOK, healthReason := s.httpHealth(port, spec.healthPath)
		if healthOK {
			item.Health, item.HealthReason = "healthy", healthReason
			item.Evidence = append(item.Evidence, "live health endpoint")
			item.Confidence += .3
		} else {
			item.Health, item.HealthReason = "unhealthy", healthReason
		}
	}
	if item.Confidence > 1 {
		item.Confidence = 1
	}
	switch {
	case item.Confidence >= .8:
		item.ConfidenceTag = "high"
	case item.Confidence >= .5:
		item.ConfidenceTag = "medium"
	default:
		item.ConfidenceTag = "low"
	}
	item.Permissions = inspectPermissions(item.Config, item.Data, item.Unit)
	item.Fingerprint = discoveryFingerprint(item)
	return item
}

func discoverySystemdUnits(spec componentDiscoverySpec) []systemdDiscovery {
	names := discoveryBinaryNames(spec)
	roots := []string{"/etc/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}
	seen := map[string]bool{}
	result := make([]systemdDiscovery, 0)
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			unit := entry.Name()
			if seen[unit] || !strings.HasSuffix(unit, ".service") {
				continue
			}
			path := filepath.Join(root, unit)
			info, err := os.Stat(path)
			if err != nil || info.IsDir() || info.Size() > 1<<20 {
				continue
			}
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			binary := systemdExecBinary(string(content), names)
			if binary == "" {
				continue
			}
			seen[unit] = true
			pid := 0
			if output, ok := commandOutput(time.Second, "systemctl", "show", unit, "-p", "MainPID", "--value"); ok {
				pid, _ = strconv.Atoi(strings.TrimSpace(output))
			}
			result = append(result, systemdDiscovery{Unit: unit, Binary: binary, PID: pid, Port: systemdDiscoveryPort(string(content), spec.port)})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Unit < result[j].Unit })
	return result
}

func discoveryBinaryNames(spec componentDiscoverySpec) map[string]bool {
	names := map[string]bool{spec.kind: true}
	for _, candidate := range spec.bins {
		names[filepath.Base(candidate)] = true
	}
	return names
}

func systemdExecBinary(content string, names map[string]bool) string {
	content = strings.ReplaceAll(content, "\\\n", " ")
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		command := strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
		command = strings.TrimLeft(command, "-+!:@")
		for _, field := range strings.Fields(command) {
			field = strings.Trim(field, `"'`)
			if names[filepath.Base(field)] && filepath.IsAbs(field) {
				return filepath.Clean(field)
			}
		}
	}
	return ""
}

func systemdDiscoveryPort(content string, fallback int) int {
	pattern := regexp.MustCompile(`--(?:web\.)?listen-address(?:=|\s+)[^\s:]*:(\d+)`)
	if match := pattern.FindStringSubmatch(content); len(match) == 2 {
		if port, err := strconv.Atoi(match[1]); err == nil && port > 0 && port <= 65535 {
			return port
		}
	}
	return fallback
}

func processMatchesDiscoverySpec(pid int, spec componentDiscoverySpec) bool {
	names := map[string]bool{}
	for _, candidate := range spec.bins {
		names[filepath.Base(candidate)] = true
	}
	names[spec.kind] = true
	if executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		if names[filepath.Base(strings.TrimSuffix(executable, " (deleted)"))] {
			return true
		}
	}
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		parts := strings.Split(string(cmdline), "\x00")
		return len(parts) > 0 && names[filepath.Base(parts[0])]
	}
	return false
}

func preferredDiscoveryPort(ports []int, preferred int) int {
	sort.Ints(ports)
	for _, port := range ports {
		if port == preferred {
			return port
		}
	}
	if len(ports) > 0 {
		return ports[0]
	}
	return preferred
}

func discoveryFingerprint(item DiscoveredComponent) string {
	value := fmt.Sprintf("%s|%d|%d|%s|%s|%s", item.Type, item.PID, item.Port, item.Binary, item.Runtime, item.Unit)
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func stopDiscoveredUnit(ctx context.Context, unit string, removed *[]string) error {
	if !regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.service$`).MatchString(unit) {
		return ErrDiscoveredNotFound
	}
	if output, err := exec.CommandContext(ctx, "systemctl", "stop", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("停止发现服务失败: %s: %w", strings.TrimSpace(string(output)), err)
	}
	_ = exec.CommandContext(ctx, "systemctl", "disable", unit).Run()
	*removed = append(*removed, "systemd:"+unit)
	return nil
}

func stopDiscoveredProcess(pid int, expectedBinary string) error {
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取发现进程失败: %w", err)
	}
	executable = strings.TrimSuffix(executable, " (deleted)")
	if expectedBinary != "" && executable != expectedBinary {
		return ErrDiscoveredNotFound
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("停止发现进程失败: %w", err)
	}
	for attempt := 0; attempt < 30; attempt++ {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("强制停止发现进程失败: %w", err)
	}
	return nil
}

type discoveredPackage struct {
	Manager string
	Name    string
}

func uninstallDiscoveredBinary(ctx context.Context, item DiscoveredComponent, removed *[]string) error {
	path := filepath.Clean(strings.TrimSuffix(item.Binary, " (deleted)"))
	if path == "." || path == "" {
		return nil
	}
	if !filepath.IsAbs(path) || !discoveryBinaryMatchesType(path, item.Type) {
		return fmt.Errorf("发现二进制路径与组件类型不匹配，拒绝卸载: %s", path)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("读取发现二进制失败: %w", err)
	}
	owner, err := discoveredPackageOwner(path)
	if err != nil {
		return err
	}
	if owner != nil {
		if !packageMatchesDiscoveryType(owner.Name, item.Type, filepath.Base(path)) {
			return fmt.Errorf("二进制 %s 属于非专用系统包 %s，拒绝自动卸载", path, owner.Name)
		}
		if err := uninstallDiscoveredPackage(ctx, *owner); err != nil {
			return err
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return fmt.Errorf("软件包 %s 卸载后二进制仍存在: %s", owner.Name, path)
		}
		*removed = append(*removed, "package:"+owner.Manager+":"+owner.Name)
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("发现路径不是普通二进制文件: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("删除独立二进制失败: %w", err)
	}
	*removed = append(*removed, path)
	return nil
}

func discoveredPackageOwner(path string) (*discoveredPackage, error) {
	if _, err := exec.LookPath("rpm"); err == nil {
		output, commandErr := exec.Command("rpm", "-qf", path).CombinedOutput()
		if commandErr == nil {
			name := strings.TrimSpace(string(output))
			if validDiscoveredPackageName(name) {
				return &discoveredPackage{Manager: "rpm", Name: name}, nil
			}
		}
	}
	if _, err := exec.LookPath("dpkg-query"); err == nil {
		output, commandErr := exec.Command("dpkg-query", "-S", path).CombinedOutput()
		if commandErr == nil {
			line := strings.TrimSpace(strings.Split(string(output), "\n")[0])
			if separator := strings.Index(line, ": "); separator > 0 {
				name := line[:separator]
				if validDiscoveredPackageName(name) {
					return &discoveredPackage{Manager: "deb", Name: name}, nil
				}
			}
		}
	}
	return nil, nil
}

func validDiscoveredPackageName(name string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9+_.:@~-]+$`).MatchString(name)
}

func packageMatchesDiscoveryType(packageName, componentType, binaryName string) bool {
	normalize := func(value string) string {
		return strings.ReplaceAll(strings.ToLower(value), "_", "-")
	}
	pkg := normalize(packageName)
	kind := normalize(componentType)
	binary := normalize(binaryName)
	return strings.Contains(pkg, kind) || strings.Contains(pkg, binary)
}

func uninstallDiscoveredPackage(ctx context.Context, owner discoveredPackage) error {
	type command struct {
		name string
		args []string
	}
	var candidates []command
	switch owner.Manager {
	case "rpm":
		candidates = []command{
			{"dnf", []string{"-y", "remove", owner.Name}},
			{"yum", []string{"-y", "remove", owner.Name}},
			{"microdnf", []string{"-y", "remove", owner.Name}},
			{"rpm", []string{"-e", owner.Name}},
		}
	case "deb":
		candidates = []command{
			{"apt-get", []string{"-y", "remove", owner.Name}},
			{"dpkg", []string{"--remove", owner.Name}},
		}
	default:
		return fmt.Errorf("不支持的软件包管理器: %s", owner.Manager)
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate.name); err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, candidate.name, candidate.args...)
		if candidate.name == "apt-get" {
			cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		}
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("使用 %s 卸载软件包 %s 失败: %s: %w", candidate.name, owner.Name, strings.TrimSpace(string(output)), err)
		}
		return nil
	}
	return fmt.Errorf("未找到可用的软件包卸载工具")
}

func discoveryBinaryMatchesType(path, componentType string) bool {
	base := filepath.Base(path)
	for _, spec := range discoverySpecs() {
		if spec.kind != componentType {
			continue
		}
		if discoveryBinaryNames(spec)[base] {
			return true
		}
	}
	return false
}

func removeRemainingDiscoveredUnit(ctx context.Context, unit string, removed *[]string) error {
	if unit == "" {
		return nil
	}
	fragment, _ := commandOutput(2*time.Second, "systemctl", "show", unit, "-p", "FragmentPath", "--value")
	fragment = filepath.Clean(strings.TrimSpace(fragment))
	if fragment == "." || fragment == "" {
		return nil
	}
	if _, err := os.Stat(fragment); os.IsNotExist(err) {
		return nil
	}
	allowed := false
	for _, root := range []string{"/etc/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
		relative, err := filepath.Rel(root, fragment)
		if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			allowed = true
			break
		}
	}
	if !allowed || filepath.Base(fragment) != unit {
		return fmt.Errorf("systemd 单元路径不安全，拒绝删除: %s", fragment)
	}
	owner, err := discoveredPackageOwner(fragment)
	if err != nil {
		return err
	}
	if owner != nil {
		return fmt.Errorf("systemd 单元仍属于软件包 %s，软件包卸载不完整", owner.Name)
	}
	if err := os.Remove(fragment); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 systemd 单元失败: %w", err)
	}
	*removed = append(*removed, fragment)
	_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	return nil
}

func verifyDiscoveredRemoval(item DiscoveredComponent, verified map[string]bool) error {
	if item.PID > 0 {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", item.PID)); !os.IsNotExist(err) {
			return fmt.Errorf("卸载验证失败：PID %d 仍存在", item.PID)
		}
		verified["pid_stopped"] = true
	}
	if item.Binary != "" {
		if _, err := os.Stat(item.Binary); !os.IsNotExist(err) {
			return fmt.Errorf("卸载验证失败：二进制仍存在 %s", item.Binary)
		}
		verified["binary_removed"] = true
	}
	if item.Unit != "" {
		state, _ := commandOutput(2*time.Second, "systemctl", "is-active", item.Unit)
		state = strings.TrimSpace(state)
		if state == "active" || state == "activating" || state == "reloading" {
			return fmt.Errorf("卸载验证失败：systemd 单元仍为 %s", state)
		}
		enabled, _ := commandOutput(2*time.Second, "systemctl", "is-enabled", item.Unit)
		enabled = strings.TrimSpace(enabled)
		if enabled == "enabled" || enabled == "enabled-runtime" || enabled == "linked" || enabled == "linked-runtime" {
			return fmt.Errorf("卸载验证失败：systemd 单元仍为 %s", enabled)
		}
		verified["unit_inactive"] = true
		verified["unit_disabled"] = true
	}
	if item.PID > 0 {
		if _, occupied := localListeners()[item.Port]; occupied {
			return fmt.Errorf("卸载验证失败：端口 %d 仍被监听", item.Port)
		}
		verified["port_released"] = true
	}
	return nil
}
func (s *OnboardingService) checkPrometheusHealth(baseURL string, now time.Time) AcceptanceCheck {
	status, _, err := s.getJSON(strings.TrimSuffix(baseURL, "/") + "/-/healthy")
	if err != nil || status < 200 || status >= 300 {
		return AcceptanceCheck{Key: "prometheus", Status: "fail", Reason: httpFailureReason("Prometheus 健康检查失败", status, err), CheckedAt: now}
	}
	return AcceptanceCheck{Key: "prometheus", Status: "pass", Reason: "Prometheus 实时健康检查成功", Evidence: map[string]interface{}{"url": baseURL, "http_status": status}, CheckedAt: now}
}

func (s *OnboardingService) checkPrometheusTargets(baseURL string, now time.Time) AcceptanceCheck {
	status, body, err := s.getJSON(strings.TrimSuffix(baseURL, "/") + "/api/v1/targets")
	if err != nil || status < 200 || status >= 300 {
		return AcceptanceCheck{Key: "targets", Status: "fail", Reason: httpFailureReason("无法读取 Prometheus targets", status, err), CheckedAt: now}
	}
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			ActiveTargets []struct {
				Health string `json:"health"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Status != "success" {
		return AcceptanceCheck{Key: "targets", Status: "fail", Reason: "Prometheus targets 响应无效", CheckedAt: now}
	}
	up := 0
	for _, target := range payload.Data.ActiveTargets {
		if target.Health == "up" {
			up++
		}
	}
	if up == 0 {
		return AcceptanceCheck{Key: "targets", Status: "fail", Reason: "没有实时状态为 up 的 active target", Evidence: map[string]int{"active": len(payload.Data.ActiveTargets), "up": up}, CheckedAt: now}
	}
	return AcceptanceCheck{Key: "targets", Status: "pass", Reason: fmt.Sprintf("%d/%d 个 active target 实时为 up", up, len(payload.Data.ActiveTargets)), Evidence: map[string]int{"active": len(payload.Data.ActiveTargets), "up": up}, CheckedAt: now}
}

func (s *OnboardingService) checkPrometheusRules(baseURL string, now time.Time) AcceptanceCheck {
	status, body, err := s.getJSON(strings.TrimSuffix(baseURL, "/") + "/api/v1/rules")
	if err != nil || status < 200 || status >= 300 {
		return AcceptanceCheck{Key: "rules", Status: "fail", Reason: httpFailureReason("无法读取 Prometheus rules", status, err), CheckedAt: now}
	}
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			Groups []struct {
				Rules []struct {
					Health string `json:"health"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Status != "success" {
		return AcceptanceCheck{Key: "rules", Status: "fail", Reason: "Prometheus rules 响应无效", CheckedAt: now}
	}
	total, healthy := 0, 0
	for _, group := range payload.Data.Groups {
		for _, rule := range group.Rules {
			total++
			if rule.Health == "ok" {
				healthy++
			}
		}
	}
	if total == 0 || healthy != total {
		return AcceptanceCheck{Key: "rules", Status: "fail", Reason: fmt.Sprintf("规则实时验收未通过：%d/%d 健康", healthy, total), Evidence: map[string]int{"total": total, "healthy": healthy}, CheckedAt: now}
	}
	return AcceptanceCheck{Key: "rules", Status: "pass", Reason: fmt.Sprintf("%d 条规则实时健康", total), Evidence: map[string]int{"total": total, "healthy": healthy}, CheckedAt: now}
}

func (s *OnboardingService) sendAcceptanceNotification(channelID uint, now time.Time) AcceptanceCheck {
	var channel models.NotificationChannel
	query := s.db.Where("enabled = ?", true)
	if channelID != 0 {
		query = query.Where("id = ?", channelID)
	}
	if err := query.Order("id ASC").First(&channel).Error; err != nil {
		return AcceptanceCheck{Key: "notifications", Status: "fail", Reason: "没有可用于实时测试的已启用通知渠道", CheckedAt: now}
	}
	message := "Prometheus 生命周期平台首次向导实时通知验收 " + now.Format(time.RFC3339)
	status, reason := deliverAcceptanceChannel(&channel, message)
	record := models.AlertNotification{AlertName: "OnboardingAcceptance", Severity: "info", ChannelID: channel.ID, ChannelType: channel.Type, Message: message, Status: status, Error: reason, CreatedAt: now}
	_ = s.db.Create(&record).Error
	if status != "sent" {
		return AcceptanceCheck{Key: "notifications", Status: "fail", Reason: "通知实时发送失败: " + reason, Evidence: map[string]interface{}{"channel_id": channel.ID, "channel_type": channel.Type}, CheckedAt: now}
	}
	return AcceptanceCheck{Key: "notifications", Status: "pass", Reason: "通知渠道实时发送成功", Evidence: map[string]interface{}{"channel_id": channel.ID, "channel_type": channel.Type, "notification_id": record.ID}, CheckedAt: now}
}

func (s *OnboardingService) recentNotificationEvidence(now time.Time) AcceptanceCheck {
	var run models.OnboardingCheckRun
	if err := s.db.Where("category = ? AND status = ? AND checked_at >= ?", "notifications", "pass", now.Add(-onboardingEvidenceFreshness)).Order("checked_at DESC").First(&run).Error; err != nil {
		return AcceptanceCheck{Key: "notifications", Status: "fail", Reason: "过去 24 小时没有成功的实时通知验收", CheckedAt: now}
	}
	return AcceptanceCheck{Key: "notifications", Status: "pass", Reason: "最近一次实时通知验收成功", Evidence: map[string]interface{}{"checked_at": run.CheckedAt, "evidence": run.Evidence}, CheckedAt: now}
}

func (s *OnboardingService) recentBackupEvidence(now time.Time) AcceptanceCheck {
	var run models.OnboardingCheckRun
	if err := s.db.Where("category = ? AND status = ? AND checked_at >= ?", "backup", "pass", now.Add(-onboardingEvidenceFreshness)).Order("checked_at DESC").First(&run).Error; err != nil {
		return AcceptanceCheck{Key: "backup", Status: "fail", Reason: "过去 24 小时没有成功的备份内容校验", CheckedAt: now}
	}
	var evidence struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if json.Unmarshal([]byte(run.Evidence), &evidence) != nil || evidence.Path == "" || evidence.Size <= 0 {
		return AcceptanceCheck{Key: "backup", Status: "fail", Reason: "最近备份校验证据无效，请重新执行验收", CheckedAt: now}
	}
	info, err := os.Stat(evidence.Path)
	if err != nil || info.IsDir() || info.Size() != evidence.Size || info.ModTime().After(run.CheckedAt.Add(time.Second)) {
		return AcceptanceCheck{Key: "backup", Status: "fail", Reason: "已验证备份在验收后被删除或变更，请重新执行验收", Evidence: evidence.Path, CheckedAt: now}
	}
	return AcceptanceCheck{Key: "backup", Status: "pass", Reason: "备份在 24 小时内完成内容校验且文件未变更", Evidence: map[string]interface{}{"path": evidence.Path, "size": evidence.Size, "checked_at": run.CheckedAt}, CheckedAt: now}
}

func (s *OnboardingService) checkFreshBackup(now time.Time) AcceptanceCheck {
	path, createdAt := s.newestBackupCandidate()
	if path == "" {
		return AcceptanceCheck{Key: "backup", Status: "fail", Reason: "未找到可读取的备份文件", CheckedAt: now}
	}
	info, err := os.Stat(path)
	if err != nil {
		return AcceptanceCheck{Key: "backup", Status: "fail", Reason: "备份文件不可访问: " + err.Error(), Evidence: path, CheckedAt: now}
	}
	if info.ModTime().After(createdAt) {
		createdAt = info.ModTime()
	}
	if now.Sub(createdAt) > onboardingEvidenceFreshness {
		return AcceptanceCheck{Key: "backup", Status: "fail", Reason: "最近备份已超过 24 小时", Evidence: map[string]interface{}{"path": path, "created_at": createdAt}, CheckedAt: now}
	}
	if strings.EqualFold(filepath.Ext(path), ".db") {
		file, openErr := os.Open(path)
		if openErr != nil {
			return AcceptanceCheck{Key: "backup", Status: "fail", Reason: "SQLite 备份不可读取: " + openErr.Error(), Evidence: path, CheckedAt: now}
		}
		header := make([]byte, 16)
		_, readErr := io.ReadFull(file, header)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || string(header) != "SQLite format 3\x00" {
			return AcceptanceCheck{Key: "backup", Status: "fail", Reason: "SQLite 备份文件头无效，不能用于恢复", Evidence: path, CheckedAt: now}
		}
	}
	checksum, size, err := hashBackup(path)
	if err != nil || size == 0 {
		reason := "备份内容为空"
		if err != nil {
			reason = "备份读取校验失败: " + err.Error()
		}
		return AcceptanceCheck{Key: "backup", Status: "fail", Reason: reason, Evidence: path, CheckedAt: now}
	}
	return AcceptanceCheck{Key: "backup", Status: "pass", Reason: "新鲜备份已完整读取并计算 SHA-256", Evidence: map[string]interface{}{"path": path, "size": size, "sha256": checksum, "created_at": createdAt}, CheckedAt: now}
}

func (s *OnboardingService) newestBackupCandidate() (string, time.Time) {
	var backups []models.Backup
	_ = s.db.Order("created_at DESC").Limit(20).Find(&backups).Error
	bestPath := ""
	var bestTime time.Time
	for _, backup := range backups {
		if backup.Path == "" {
			continue
		}
		if info, statErr := os.Stat(backup.Path); statErr == nil && !info.IsDir() && backup.CreatedAt.After(bestTime) {
			bestPath, bestTime = backup.Path, backup.CreatedAt
		}
	}
	patterns := []string{filepath.Join(s.basePath, "backups", "*.db"), filepath.Join(s.basePath, "backups", "platform-db-*.db")}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, candidate := range matches {
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() && info.ModTime().After(bestTime) {
				bestPath, bestTime = candidate, info.ModTime()
			}
		}
	}
	return bestPath, bestTime
}

func (s *OnboardingService) recordCheck(check AcceptanceCheck) {
	if s.db == nil {
		return
	}
	evidence, _ := json.Marshal(check.Evidence)
	_ = s.db.Create(&models.OnboardingCheckRun{Category: check.Key, Status: check.Status, Reason: check.Reason, Evidence: string(evidence), CheckedAt: check.CheckedAt}).Error
}

func (s *OnboardingService) runningComponent(kind models.ComponentType) (*models.Component, error) {
	var component models.Component
	err := s.db.Where("type = ? AND status <> ?", kind, "removed").Preload("Host").Order("CASE WHEN status = 'running' THEN 0 ELSE 1 END, id ASC").First(&component).Error
	return &component, err
}

func (s *OnboardingService) getJSON(url string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, err
}

func (s *OnboardingService) httpHealth(port int, healthPath string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, healthPath), nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return true, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

func (s *OnboardingService) availableOfflinePackages() (int, []string) {
	var packages []models.Package
	_ = s.db.Order("type ASC").Find(&packages).Error
	types := make([]string, 0)
	seen := map[string]bool{}
	count := 0
	for _, pkg := range packages {
		if pkg.StoragePath == "" {
			continue
		}
		info, err := os.Stat(pkg.StoragePath)
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		count++
		kind := string(pkg.Type)
		if !seen[kind] {
			seen[kind] = true
			types = append(types, kind)
		}
	}
	return count, types
}

func localOS() (string, string) {
	name, version := runtime.GOOS, runtime.GOOS
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return name, version
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			values[parts[0]] = strings.Trim(parts[1], "\"")
		}
	}
	if values["ID"] != "" {
		name = "linux"
	}
	if values["PRETTY_NAME"] != "" {
		version = values["PRETTY_NAME"]
	}
	return name, version
}

func commandOutput(timeout time.Duration, name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(output), err == nil
}

func localMemoryBytes() uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

func availableDiskBytes(path string) (uint64, error) {
	if err := os.MkdirAll(path, 0755); err != nil {
		return 0, err
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func probeWritable(path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(path, ".onboarding-write-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.WriteString("ok"); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func portAvailable(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	return listener.Close() == nil
}

func localListeners() map[int]int {
	result := map[int]int{}
	output, _ := commandOutput(2*time.Second, "ss", "-ltnpH")
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		address := fields[3]
		idx := strings.LastIndex(address, ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.Atoi(strings.Trim(address[idx+1:], "[]"))
		if err != nil {
			continue
		}
		pid := 0
		if marker := strings.Index(line, "pid="); marker >= 0 {
			rest := line[marker+4:]
			end := strings.IndexAny(rest, ",)")
			if end >= 0 {
				rest = rest[:end]
			}
			pid, _ = strconv.Atoi(rest)
		}
		result[port] = pid
	}
	return result
}

func findArgument(args []string, names ...string) string {
	for i, arg := range args {
		for _, name := range names {
			if arg == name && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(arg, name+"=") {
				return strings.TrimPrefix(arg, name+"=")
			}
		}
	}
	return ""
}

func unitForPID(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	for _, field := range strings.FieldsFunc(string(data), func(r rune) bool { return r == '/' || r == ':' || r == '\n' }) {
		if strings.HasSuffix(field, ".service") {
			return field
		}
	}
	return ""
}

func firstExecutable(candidates []string) string {
	for _, candidate := range candidates {
		if strings.ContainsRune(candidate, os.PathSeparator) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				return candidate
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}

func firstExisting(candidates []string) string {
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func binaryVersion(binary string) string {
	output, _ := commandOutput(2*time.Second, binary, "--version")
	line := strings.TrimSpace(strings.Split(output, "\n")[0])
	if len(line) > 256 {
		line = line[:256]
	}
	return line
}

func inspectPermissions(configPath, dataPath, unit string) DiscoveryPermissions {
	permissions := DiscoveryPermissions{}
	if configPath != "" {
		if file, err := os.Open(configPath); err == nil {
			permissions.ConfigReadable = true
			file.Close()
		}
		permissions.ConfigWritable = writablePath(configPath)
	}
	if dataPath != "" {
		if file, err := os.Open(dataPath); err == nil {
			permissions.DataReadable = true
			file.Close()
		}
		permissions.DataWritable = writablePath(dataPath)
	}
	if unit != "" {
		output, ok := commandOutput(time.Second, "systemctl", "show", unit, "-p", "FragmentPath", "--value")
		permissions.ServiceWritable = ok && writablePath(strings.TrimSpace(output))
	}
	return permissions
}

func writablePath(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	mode := info.Mode().Perm()
	if os.Geteuid() == 0 {
		return true
	}
	return mode&0200 != 0
}

func componentURL(component *models.Component, fallbackPort int) string {
	if component.ExternalURL != "" {
		return strings.TrimSuffix(component.ExternalURL, "/")
	}
	if component.InternalURL != "" {
		return strings.TrimSuffix(component.InternalURL, "/")
	}
	port := component.Port
	if port == 0 {
		port = fallbackPort
	}
	host := component.Host.IP
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

func deliverAcceptanceChannel(channel *models.NotificationChannel, message string) (string, string) {
	if strings.EqualFold(channel.Type, "email") {
		settings, recipient, err := config.ResolveEmailChannel(*channel)
		if err != nil {
			return "failed", err.Error()
		}
		body := "Content-Type: text/plain; charset=UTF-8\r\nSubject: 【Prometheus 监控平台】首次向导验收\r\nFrom: " + settings.From + "\r\nTo: " + recipient + "\r\n\r\n" + message
		auth := smtp.PlainAuth("", settings.Username, settings.Password, settings.Host)
		if err := smtp.SendMail(settings.Smarthost(), auth, settings.From, []string{recipient}, []byte(body)); err != nil {
			return "failed", "SMTP 发送失败"
		}
		return "sent", ""
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(channel.Config), &cfg); err != nil {
		return "failed", "配置不是有效 JSON"
	}
	endpoint := mapString(cfg, "url", "webhook")
	if endpoint == "" {
		return "failed", "webhook URL 为空"
	}
	var payload interface{}
	switch strings.ToLower(channel.Type) {
	case "dingtalk", "wechat":
		payload = map[string]interface{}{"msgtype": "text", "text": map[string]string{"content": message}}
	case "slack":
		payload = map[string]interface{}{"blocks": []map[string]interface{}{{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": message}}}}
	case "webhook":
		payload = map[string]string{"text": message, "alert": "OnboardingAcceptance", "severity": "info"}
	default:
		return "failed", "不支持的通知渠道类型"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "failed", err.Error()
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: utils.SafeDialContext}}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "failed", err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "failed", fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return "sent", ""
}

func mapString(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func hashBackup(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	return hex.EncodeToString(hash.Sum(nil)), size, err
}

func httpFailureReason(prefix string, status int, err error) string {
	if err != nil {
		return prefix + ": " + err.Error()
	}
	return fmt.Sprintf("%s: HTTP %d", prefix, status)
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func preflightReason(report PreflightReport) string {
	if report.Ready {
		return fmt.Sprintf("预检通过（%d 项警告）", report.Summary["warn"])
	}
	return fmt.Sprintf("%d 个阻断项待处理", len(report.Blockers))
}

func combinedStatus(left, right AcceptanceCheck) string {
	if left.Status == "pass" && right.Status == "pass" {
		return "pass"
	}
	return "fail"
}
