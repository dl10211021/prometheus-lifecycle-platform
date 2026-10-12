package deployer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/network"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

// PackageDownloadProgress describes one observable stage of an official
// package download. Total is zero when the source cannot report a size.
type PackageDownloadProgress struct {
	Stage      string
	Detail     string
	Downloaded int64
	Total      int64
}

type PackageDownloadProgressFunc func(PackageDownloadProgress)

func reportPackageDownloadProgress(progress PackageDownloadProgressFunc, stage, detail string, downloaded, total int64) {
	if progress != nil {
		progress(PackageDownloadProgress{
			Stage:      stage,
			Detail:     detail,
			Downloaded: downloaded,
			Total:      total,
		})
	}
}

// OfficialPackageURL returns the GitHub release download URL for a component version.
func OfficialPackageURL(compType, version string) (string, string, error) {
	if !utils.IsValidComponentType(compType) {
		return "", "", fmt.Errorf("unsupported official component type %q", compType)
	}
	if !utils.ValidVersion(version) {
		return "", "", fmt.Errorf("invalid component version")
	}
	// returns (url, archiveName)
	switch compType {
	case "prometheus":
		v := version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		name := fmt.Sprintf("prometheus-%s.linux-amd64.tar.gz", strings.TrimPrefix(v, "v"))
		return fmt.Sprintf("https://github.com/prometheus/prometheus/releases/download/%s/%s", v, name), name, nil
	case "node_exporter":
		v := strings.TrimPrefix(version, "v")
		name := fmt.Sprintf("node_exporter-%s.linux-amd64.tar.gz", v)
		return fmt.Sprintf("https://github.com/prometheus/node_exporter/releases/download/v%s/%s", v, name), name, nil
	case "alertmanager":
		v := version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		name := fmt.Sprintf("alertmanager-%s.linux-amd64.tar.gz", strings.TrimPrefix(v, "v"))
		return fmt.Sprintf("https://github.com/prometheus/alertmanager/releases/download/%s/%s", v, name), name, nil
	case "blackbox_exporter":
		v := version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		name := fmt.Sprintf("blackbox_exporter-%s.linux-amd64.tar.gz", strings.TrimPrefix(v, "v"))
		return fmt.Sprintf("https://github.com/prometheus/blackbox_exporter/releases/download/%s/%s", v, name), name, nil
	case "pushgateway":
		v := version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		name := fmt.Sprintf("pushgateway-%s.linux-amd64.tar.gz", strings.TrimPrefix(v, "v"))
		return fmt.Sprintf("https://github.com/prometheus/pushgateway/releases/download/%s/%s", v, name), name, nil
	case "loki":
		v := version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		name := "loki-linux-amd64.zip"
		return fmt.Sprintf("https://github.com/grafana/loki/releases/download/%s/%s", v, name), name, nil
	case "promtail":
		v := version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		name := "promtail-linux-amd64.zip"
		return fmt.Sprintf("https://github.com/grafana/loki/releases/download/%s/%s", v, name), name, nil
	case "thanos":
		v := version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		name := fmt.Sprintf("thanos-%s.linux-amd64.tar.gz", strings.TrimPrefix(v, "v"))
		return fmt.Sprintf("https://github.com/thanos-io/thanos/releases/download/%s/%s", v, name), name, nil
	case "grafana":
		v := strings.TrimPrefix(version, "v")
		name := fmt.Sprintf("grafana-%s.linux-amd64.tar.gz", v)
		return fmt.Sprintf("https://dl.grafana.com/oss/release/%s", name), name, nil
	default:
		return "", "", fmt.Errorf("no official package source for component type %s", compType)
	}
}

// BuiltinVersions maps each component type to its built-in default version,
// downloaded once at startup so deployments work offline afterwards.
var BuiltinVersions = map[string]string{
	"prometheus":        "v2.53.0",
	"alertmanager":      "v0.27.0",
	"node_exporter":     "1.8.2",
	"blackbox_exporter": "v0.24.0",
	"pushgateway":       "v1.6.2",
	"loki":              "v3.2.0",
	"promtail":          "v3.2.0",
	"thanos":            "v0.39.2",
	"grafana":           "11.1.0",
}

// DefaultVersion returns the built-in default version for a component type.
func DefaultVersion(compType string) string {
	return BuiltinVersions[compType]
}

// EnsureBuiltinPackages pre-downloads the built-in component packages in the
// background so later deployments use the local packages without network access.
func EnsureBuiltinPackages(db *gorm.DB, basePath string) {
	if db == nil {
		return
	}
	go func() {
		for compType, version := range BuiltinVersions {
			if _, err := FindAvailablePackage(db, basePath, compType, version); err == nil {
				continue
			}
			log.Printf("[Packages] Pre-downloading built-in package: %s %s", compType, version)
			if _, err := DownloadOfficialPackage(context.Background(), db, basePath, compType, version, nil); err != nil {
				log.Printf("[Packages] Built-in download failed for %s %s: %v (deployment will try again or require offline package)", compType, version, err)
			}
		}
		log.Printf("[Packages] Built-in package pre-download finished")
	}()
}

// officialArchivePrefixes lists the official archive filename prefixes per
// component type, used to reverse-map a file in {basePath}/packages back to
// its (type, version).
var officialArchivePrefixes = []struct {
	compType string
	prefix   string
}{
	{"prometheus", "prometheus-"},
	{"alertmanager", "alertmanager-"},
	{"node_exporter", "node_exporter-"},
	{"blackbox_exporter", "blackbox_exporter-"},
	{"pushgateway", "pushgateway-"},
	{"thanos", "thanos-"},
	{"grafana", "grafana-"},
}

// ScanLocalPackages registers official archives present in {basePath}/packages
// that have no Package record yet. This heals environments where the database
// was rebuilt or restored separately from the package files: without records,
// deployments ignore the local archives and hit the network instead.
func ScanLocalPackages(db *gorm.DB, basePath string) {
	if db == nil {
		return
	}
	pkgDir := filepath.Join(basePath, "packages")
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return
	}
	registered := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := e.Name()
		compType, version := parseOfficialArchiveName(name)
		if compType == "" {
			continue
		}
		storagePath := filepath.Join(pkgDir, name)
		if _, err := findPackageRecord(db, compType, version); err == nil {
			continue
		}
		// Accept the v-prefix variant too so "1.8.2" and "v1.8.2" never
		// produce duplicate records.
		alt := "v" + version
		if strings.HasPrefix(version, "v") {
			alt = strings.TrimPrefix(version, "v")
		}
		if _, err := findPackageRecord(db, compType, alt); err == nil {
			continue
		}
		info, err := os.Stat(storagePath)
		if err != nil || info.Size() == 0 {
			continue
		}
		if _, err := packageRecordFor(db, compType, version, storagePath, info.Size()); err != nil {
			log.Printf("[Packages] scan: failed to register %s: %v", name, err)
			continue
		}
		registered++
		log.Printf("[Packages] scan: registered existing archive %s (%s %s)", name, compType, version)
	}
	if registered > 0 {
		log.Printf("[Packages] scan: registered %d previously untracked archive(s)", registered)
	}
}

// parseOfficialArchiveName recovers only explicit version identities. Unversioned
// upstream ZIP names are ambiguous and must not be registered as a default release.
func parseOfficialArchiveName(name string) (string, string) {
	for _, p := range officialArchivePrefixes {
		if strings.HasPrefix(name, p.prefix) && strings.HasSuffix(name, ".linux-amd64.tar.gz") {
			version := strings.TrimSuffix(strings.TrimPrefix(name, p.prefix), ".linux-amd64.tar.gz")
			if !utils.ValidVersion(version) {
				continue
			}
			return p.compType, version
		}
	}
	for _, compType := range []string{"loki", "promtail"} {
		if strings.HasPrefix(name, compType+"-") && strings.HasSuffix(name, ".linux-amd64.zip") {
			version := strings.TrimSuffix(strings.TrimPrefix(name, compType+"-"), ".linux-amd64.zip")
			if utils.ValidVersion(version) {
				return compType, version
			}
		}
	}
	return "", ""
}

// RemoveUnusedPackageCache removes extracted binary artifacts only when no
// deployed component or second package record still references the version.
func RemoveUnusedPackageCache(db *gorm.DB, basePath string, pkg *models.Package) error {
	if db == nil || pkg == nil || (pkg.Format != "binary" && pkg.Format != "zip") {
		return nil
	}
	var componentCount int64
	if err := db.Model(&models.Component{}).
		Where("type = ? AND version = ?", pkg.Type, pkg.Version).
		Count(&componentCount).Error; err != nil {
		return err
	}
	if componentCount > 0 {
		return nil
	}
	var packageCount int64
	if err := db.Model(&models.Package{}).
		Where("id <> ? AND type = ? AND version = ? AND format IN ?", pkg.ID, pkg.Type, pkg.Version, []string{"binary", "zip"}).
		Count(&packageCount).Error; err != nil {
		return err
	}
	if packageCount > 0 {
		return nil
	}
	if basePath == "" {
		basePath = "/opt/promlifecycle"
	}
	names := map[string]struct{}{}
	if utils.SafePathSegment(pkg.Name) {
		names[pkg.Name] = struct{}{}
	}
	if spec, err := binarySpecFor(pkg.Type); err == nil && utils.SafePathSegment(spec.BinName) {
		names[spec.BinName] = struct{}{}
	} else {
		var definition models.ComponentDefinition
		if db.Where("type = ?", pkg.Type).First(&definition).Error == nil && utils.SafePathSegment(definition.BinName) {
			names[definition.BinName] = struct{}{}
		}
	}
	if !utils.SafePathSegment(pkg.Version) {
		return nil
	}
	binRoot := filepath.Join(basePath, "bin")
	for name := range names {
		candidate := filepath.Join(binRoot, name+"-"+pkg.Version)
		if filepath.Dir(candidate) != binRoot {
			return fmt.Errorf("invalid extracted package cache path")
		}
		if err := os.RemoveAll(candidate); err != nil {
			return fmt.Errorf("remove extracted package cache: %w", err)
		}
	}
	return nil
}

// it as a binary package record (used by deployments when no local package exists,
// and by the 包管理 page). Returns the saved package.
func DownloadOfficialPackage(ctx context.Context, db *gorm.DB, basePath, compType, version string, progress PackageDownloadProgressFunc) (models.Package, error) {
	if !utils.IsValidComponentType(compType) {
		return models.Package{}, fmt.Errorf("unsupported official component type %q", compType)
	}
	if _, ok := BuiltinVersions[compType]; !ok {
		return models.Package{}, fmt.Errorf("no official package source for component type %s", compType)
	}
	if !utils.ValidVersion(version) {
		return models.Package{}, fmt.Errorf("invalid component version")
	}
	lockKey := filepath.Clean(basePath) + "/" + compType + "/" + strings.TrimPrefix(version, "v")
	value, _ := packageAcquisitionLocks.LoadOrStore(lockKey, make(chan struct{}, 1))
	lock := value.(chan struct{})
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
	case <-ctx.Done():
		return models.Package{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return models.Package{}, err
	}
	if pkg, err := FindAvailablePackage(db, basePath, compType, version); err == nil {
		reportPackageDownloadProgress(progress, "安装包已入库", filepath.Base(pkg.StoragePath), pkg.Size, pkg.Size)
		return pkg, nil
	}
	// Local archives are usable offline too, but never erase evidence of a
	// checksum failure by blessing the same corrupted file with a fresh hash.
	_, archiveName, _ := OfficialPackageURL(compType, version)
	storagePath := filepath.Join(basePath, "packages", officialStorageName(compType, version, archiveName))
	existing, recordErr := findPackageRecord(db, compType, version)
	if recordErr != nil && !errors.Is(recordErr, gorm.ErrRecordNotFound) {
		return models.Package{}, recordErr
	}
	if errors.Is(recordErr, gorm.ErrRecordNotFound) || existing.StoragePath != storagePath {
		if info, err := os.Lstat(storagePath); err == nil {
			candidate := models.Package{Type: models.ComponentType(compType), Format: "binary", StoragePath: storagePath, Size: info.Size()}
			if ValidatePackage(basePath, candidate) == nil {
				return packageRecordFor(db, compType, version, storagePath, info.Size())
			}
		}
	}
	reportPackageDownloadProgress(progress, "检查网络", "检测官方下载网络", 0, 0)
	// Offline environments must fail fast with a clear message instead of
	// waiting out multi-minute download timeouts.
	if !network.Available() {
		return models.Package{}, fmt.Errorf("%w：无法在线下载官方包（请导入 %s %s 离线包）", ErrPackageOffline, compType, version)
	}
	pkg, err := downloadFromGitHub(ctx, db, basePath, compType, version, progress)
	if err != nil {
		return models.Package{}, fmt.Errorf("官方下载失败且平台禁止 Docker 镜像回退；请配置下载镜像、HTTPS_PROXY，或通过包管理上传离线包: %w", err)
	}
	reportPackageDownloadProgress(progress, "完成", "官方包已保存", pkg.Size, pkg.Size)
	return pkg, nil
}

func downloadFromGitHub(ctx context.Context, db *gorm.DB, basePath, compType, version string, progress PackageDownloadProgressFunc) (models.Package, error) {
	url, archiveName, err := OfficialPackageURL(compType, version)
	if err != nil {
		return models.Package{}, err
	}
	reportPackageDownloadProgress(progress, "准备下载", archiveName, 0, 0)
	pkgDir := filepath.Join(basePath, "packages")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		return models.Package{}, err
	}
	storagePath, err := safePackageStoragePath(pkgDir, officialStorageName(compType, version, archiveName))
	if err != nil {
		return models.Package{}, err
	}

	// Optional user-provided URL-prefix mirror wins first (e.g. a company proxy
	// or a reachable mirror of dl.grafana.com). Then GitHub mirrors, then direct.
	proxies := []string{}
	if m := strings.TrimSpace(os.Getenv("PROMLIFECYCLE_DOWNLOAD_MIRROR")); m != "" {
		proxies = append(proxies, strings.TrimSuffix(m, "/")+"/")
	}
	if strings.HasPrefix(url, "https://github.com/") {
		proxies = append(proxies, "https://ghfast.top/", "https://gh-proxy.com/", "")
	} else {
		proxies = append(proxies, "") // non-GitHub sources (e.g. dl.grafana.com) are fetched directly
	}
	var lastErr error
	for _, proxy := range proxies {
		if err := ctx.Err(); err != nil {
			return models.Package{}, err
		}
		dlURL := proxy + url
		parsedURL, parseErr := neturl.Parse(dlURL)
		if parseErr != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil {
			lastErr = fmt.Errorf("download source must be an absolute HTTPS URL")
			continue
		}
		source := downloadSourceName(dlURL)
		reportPackageDownloadProgress(progress, "连接下载源", source, 0, 0)
		client := officialDownloadHTTPClient()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := client.Do(request)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", dlURL, err)
			reportPackageDownloadProgress(progress, "切换下载源", source+" 连接失败，尝试下一来源", 0, 0)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: HTTP %d", dlURL, resp.StatusCode)
			reportPackageDownloadProgress(progress, "切换下载源", fmt.Sprintf("%s 返回 HTTP %d，尝试下一来源", source, resp.StatusCode), 0, 0)
			continue
		}
		// Unique temp name per downloader: concurrent downloads of the same
		// package must never share a temp file (truncation/interleaving
		// corrupts both). Rename is atomic, so the last finisher wins with a
		// complete archive.
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			resp.Body.Close()
			return models.Package{}, err
		}
		tmpPath := storagePath + "." + hex.EncodeToString(rnd[:]) + ".downloading"
		out, err := os.Create(tmpPath)
		if err != nil {
			resp.Body.Close()
			return models.Package{}, err
		}
		total := resp.ContentLength
		reportPackageDownloadProgress(progress, "下载文件", source+" / "+archiveName, 0, total)
		progressWriter := &packageDownloadProgressWriter{
			progress: progress,
			detail:   source + " / " + archiveName,
			total:    total,
			next:     1 << 20,
		}
		written, err := io.Copy(io.MultiWriter(out, progressWriter), io.LimitReader(resp.Body, maxCustomPackageBytes+1))
		progressWriter.finish()
		closeErr := out.Close()
		resp.Body.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil && written > maxCustomPackageBytes {
			err = fmt.Errorf("package exceeds 5GB limit")
		}
		if err != nil {
			os.Remove(tmpPath)
			lastErr = err
			continue
		}
		if err := validatePackageArchive(tmpPath, compType, "binary"); err != nil {
			os.Remove(tmpPath)
			lastErr = err
			continue
		}
		reportPackageDownloadProgress(progress, "校验文件", "计算 SHA-256 并保存文件", written, written)
		if err := os.Rename(tmpPath, storagePath); err != nil {
			os.Remove(tmpPath)
			return models.Package{}, err
		}
		reportPackageDownloadProgress(progress, "保存记录", "写入离线包数据库记录", written, written)
		pkg, err := packageRecordFor(db, compType, version, storagePath, written)
		if err != nil {
			return models.Package{}, err
		}
		return pkg, nil
	}
	return models.Package{}, fmt.Errorf(
		"download failed: %w；官方地址 %s。可设置 PROMLIFECYCLE_DOWNLOAD_MIRROR=<镜像前缀> 或 HTTPS_PROXY 后重启服务，或手动下载该文件后通过「包管理→上传自定义包」导入",
		lastErr, url)
}
func officialDownloadHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			Proxy:       http.ProxyFromEnvironment,
			DialContext: utils.SafeDialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "https" {
				if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" {
					return fmt.Errorf("refusing HTTPS downgrade redirect")
				}
				return fmt.Errorf("unsupported redirect scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

type packageDownloadProgressWriter struct {
	progress   PackageDownloadProgressFunc
	detail     string
	downloaded int64
	total      int64
	next       int64
	reported   int64
}

func (w *packageDownloadProgressWriter) Write(p []byte) (int, error) {
	w.downloaded += int64(len(p))
	if w.downloaded >= w.next {
		reportPackageDownloadProgress(w.progress, "下载文件", w.detail, w.downloaded, w.total)
		w.reported = w.downloaded
		w.next = w.downloaded + 1<<20
	}
	return len(p), nil
}

func (w *packageDownloadProgressWriter) finish() {
	if w.downloaded != w.reported {
		reportPackageDownloadProgress(w.progress, "下载文件", w.detail, w.downloaded, w.total)
		w.reported = w.downloaded
	}
}

func downloadSourceName(rawURL string) string {
	parsed, err := neturl.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return "官方下载源"
	}
	return parsed.Hostname()
}

func safePackageStoragePath(packageDir, filename string) (string, error) {
	if !utils.SafePathSegment(filename) {
		return "", fmt.Errorf("unsafe package filename %q", filename)
	}
	path := filepath.Join(packageDir, filename)
	if !utils.PathWithinBase(packageDir, path) {
		return "", fmt.Errorf("package path escapes package directory")
	}
	return path, nil
}

// packageRecordMu serializes find-then-create of official binary Package
// records. The platform is the single-process owner of the SQLite database,
// so an in-process mutex fully closes the TOCTOU window between concurrent
// downloaders (startup pre-downloader vs. deployment vs. manual download).
// A unique index on (type, version, format) was rejected: it would also
// reject legitimate repeated custom uploads of the same type/version.
var packageRecordMu sync.Mutex

var packageAcquisitionLocks sync.Map
var ErrPackageOffline = errors.New("离线环境")

// createOfficialPackageRecordLocked records newly verified bytes, repairing an
// existing version's stale path/checksum instead of returning the broken record.
// Callers MUST hold packageRecordMu.
func createOfficialPackageRecordLocked(db *gorm.DB, pkg models.Package) (models.Package, error) {
	if existing, err := findPackageRecord(db, string(pkg.Type), pkg.Version); err == nil {
		existing.StoragePath, existing.Size, existing.Checksum, existing.Format = pkg.StoragePath, pkg.Size, pkg.Checksum, pkg.Format
		if err := db.Save(&existing).Error; err != nil {
			return models.Package{}, err
		}
		return existing, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Package{}, err
	}
	if err := db.Create(&pkg).Error; err != nil {
		return models.Package{}, err
	}
	return pkg, nil
}

// createOfficialPackageRecord is the lock-acquiring wrapper for callers that
// do not already hold packageRecordMu.
func createOfficialPackageRecord(db *gorm.DB, pkg models.Package) (models.Package, error) {
	packageRecordMu.Lock()
	defer packageRecordMu.Unlock()
	return createOfficialPackageRecordLocked(db, pkg)
}

// packageRecordFor returns the Package DB record for a downloaded archive,
// creating it when missing.
func packageRecordFor(db *gorm.DB, compType, version, storagePath string, size int64) (models.Package, error) {
	if err := validatePackageArchive(storagePath, compType, "binary"); err != nil {
		return models.Package{}, err
	}
	checksum, err := calcFileSHA256(storagePath)
	if err != nil {
		return models.Package{}, err
	}
	pkg := models.Package{
		Name:        compType + "-" + version + " (official)",
		Type:        models.ComponentType(compType),
		Version:     version,
		Format:      "binary",
		Size:        size,
		Checksum:    checksum,
		StoragePath: storagePath,
		Description: "Official release downloaded for offline deployment",
	}
	if db == nil {
		return pkg, nil
	}
	return createOfficialPackageRecord(db, pkg)
}

// findPackageRecord looks up an existing binary Package record while treating
// v1.2.3 and 1.2.3 as the same permanent installation package.
func findPackageRecord(db *gorm.DB, compType, version string) (models.Package, error) {
	if db == nil {
		return models.Package{}, gorm.ErrRecordNotFound
	}
	normalized := strings.TrimPrefix(version, "v")
	variants := []string{version, normalized, "v" + normalized}
	var pkg models.Package
	err := db.Where("type = ? AND version IN ? AND format IN ?", compType, variants, []string{"binary", "zip"}).
		Order("id DESC").First(&pkg).Error
	return pkg, err
}

func calcFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
