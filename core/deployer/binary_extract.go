package deployer

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/network"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

// extractBinary returns the component binary (or the whole install tree for
// FullDir components like grafana). Offline packages uploaded via the package
// manager are preferred; otherwise the binary is extracted from the official
// docker image (requires network or a locally loaded image).
func (d *BinaryDeployer) extractBinary(ctx context.Context, spec BinarySpec, version string) (string, error) {
	if !utils.ValidVersion(version) {
		return "", fmt.Errorf("invalid component version")
	}
	cacheDir := filepath.Join(d.basePath, "bin")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("create bin cache dir: %w", err)
	}
	cacheKey := spec.BinName + "-" + version

	// FullDir components (grafana): the whole tree must come from an offline package
	if spec.FullDir {
		dir := filepath.Join(cacheDir, cacheKey)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir, nil // already extracted
		}
		pkgPath, err := d.findLocalBinaryPackage(spec, version)
		if err != nil && d.db != nil {
			// Try to download the official package automatically (grafana can
			// not be extracted from a docker image because it needs the full
			// public/ assets tree).
			log.Printf("[Deploy] No local package for %s %s, downloading official package...", spec.ComponentType, version)
			if _, derr := DownloadOfficialPackage(ctx, d.db, d.basePath, string(spec.ComponentType), version, nil); derr == nil {
				pkgPath, err = d.findLocalBinaryPackage(spec, version)
			}
		}
		if err != nil {
			return "", fmt.Errorf("%s %s 无离线包且在线下载失败: %w；本机可用离线版本: %s", spec.BinName, version, err, d.availableVersions(spec))
		}
		tempDir := dir + ".extracting"
		_ = os.RemoveAll(tempDir)
		if err := extractFullDirFromPackage(pkgPath, tempDir, spec); err != nil {
			_ = os.RemoveAll(tempDir)
			return "", fmt.Errorf("extract %s package: %w", spec.BinName, err)
		}
		_ = os.RemoveAll(dir)
		if err := os.Rename(tempDir, dir); err != nil {
			_ = os.RemoveAll(tempDir)
			return "", fmt.Errorf("activate extracted %s package: %w", spec.BinName, err)
		}
		return dir, nil
	}

	binaryPath := filepath.Join(cacheDir, cacheKey)
	if _, err := os.Stat(binaryPath); err == nil {
		if err := validateExtractedBinary(ctx, spec, binaryPath); err == nil {
			return binaryPath, nil
		}
		_ = os.Remove(binaryPath)
	}

	// 1. Prefer an offline package uploaded through the package manager (format=binary)
	if pkgPath, err := d.findLocalBinaryPackage(spec, version); err == nil {
		if err := extractBinaryFromPackage(pkgPath, spec.BinName, binaryPath, spec.ArchiveFormat); err != nil {
			return "", fmt.Errorf("extract from offline package: %w", err)
		}
		if err := validateExtractedBinary(ctx, spec, binaryPath); err != nil {
			_ = os.Remove(binaryPath)
			return "", err
		}
		return binaryPath, nil
	}
	if spec.Custom {
		pkgPath, err := d.downloadCustomPackage(ctx, spec, version)
		if err != nil {
			return "", err
		}
		if err := extractBinaryFromPackage(pkgPath, spec.BinName, binaryPath, spec.ArchiveFormat); err != nil {
			return "", fmt.Errorf("extract from custom package: %w", err)
		}
		return binaryPath, nil
	}

	// 1b. No local package: try to download the official release automatically (online)
	if d.db != nil {
		log.Printf("[Deploy] No local package for %s %s, downloading official package...", spec.ComponentType, version)
		if _, err := DownloadOfficialPackage(ctx, d.db, d.basePath, string(spec.ComponentType), version, nil); err == nil {
			if pkgPath, err := d.findLocalBinaryPackage(spec, version); err == nil {
				if err := extractBinaryFromPackage(pkgPath, spec.BinName, binaryPath, spec.ArchiveFormat); err != nil {
					return "", fmt.Errorf("extract from downloaded package: %w", err)
				}
				if err := validateExtractedBinary(ctx, spec, binaryPath); err != nil {
					_ = os.Remove(binaryPath)
					return "", err
				}
				return binaryPath, nil
			}
		}
	}

	return "", fmt.Errorf("未找到兼容的 %s %s 离线包；平台禁止 Docker 镜像回退，请通过包管理上传官方或目标系统兼容包；本机可用离线版本: %s", spec.BinName, version, d.availableVersions(spec))
}

// ValidationTool extracts the native config checker shipped in the same
// verified offline package as the managed component.
func (d *BinaryDeployer) ValidationTool(ctx context.Context, component *models.Component) (string, error) {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return "", err
	}
	if spec.ValidationTool == "" {
		return "", fmt.Errorf("%s has no native validation tool", component.Type)
	}
	if !utils.ValidVersion(component.Version) {
		return "", fmt.Errorf("invalid component version")
	}
	target := filepath.Join(d.basePath, "bin", spec.ValidationTool+"-"+component.Version)
	if info, statErr := os.Stat(target); statErr == nil && info.Mode().IsRegular() {
		return target, nil
	}
	packagePath, err := d.findLocalBinaryPackage(spec, component.Version)
	if err != nil {
		return "", fmt.Errorf("find %s in offline package: %w", spec.ValidationTool, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return "", err
	}
	staged := target + ".extracting"
	_ = os.Remove(staged)
	if err := extractBinaryFromPackage(packagePath, spec.ValidationTool, staged, spec.ArchiveFormat); err != nil {
		_ = os.Remove(staged)
		if component.Type == models.ComponentPrometheus {
			return d.extractBinary(ctx, spec, component.Version)
		}
		return "", fmt.Errorf("extract %s from offline package: %w", spec.ValidationTool, err)
	}
	if err := os.Chmod(staged, 0755); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(checkCtx, target, "--version").CombinedOutput(); err != nil {
		_ = os.Remove(target)
		return "", fmt.Errorf("%s failed self-check: %s: %w", spec.ValidationTool, strings.TrimSpace(string(output)), err)
	}
	return target, nil
}

func validateExtractedBinary(ctx context.Context, spec BinarySpec, binaryPath string) error {
	if spec.ComponentType != models.ComponentLoki && spec.ComponentType != models.ComponentPromtail {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(checkCtx, binaryPath, "-version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s 二进制与目标系统不兼容或无法执行: %s: %w", spec.BinName, strings.TrimSpace(string(output)), err)
	}
	return nil
}

// availableVersions lists the component versions that have a usable offline
// binary package on this platform, for actionable error messages.
func (d *BinaryDeployer) availableVersions(spec BinarySpec) string {
	if d.db == nil {
		return "无"
	}
	var pkgs []models.Package
	if err := d.db.Where("type = ? AND format IN ? AND storage_path <> ''", string(spec.ComponentType), []string{"binary", "zip"}).
		Order("version DESC").Find(&pkgs).Error; err != nil {
		return "无"
	}
	seen := map[string]bool{}
	var avail []string
	for _, p := range pkgs {
		if seen[p.Version] || ValidatePackage(d.basePath, p) != nil {
			continue
		}
		seen[p.Version] = true
		avail = append(avail, p.Version)
	}
	sort.Strings(avail)
	if len(avail) == 0 {
		return "无"
	}
	return strings.Join(avail, ", ")
}

// fileExists reports whether a file exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// findLocalBinaryPackage looks up an uploaded offline package for the component+version.
// Version matching ignores the leading "v" so registry-style and official-tag style versions match.
func (d *BinaryDeployer) findLocalBinaryPackage(spec BinarySpec, version string) (string, error) {
	pkg, err := FindAvailablePackage(d.db, d.basePath, string(spec.ComponentType), version)
	if err != nil {
		return "", fmt.Errorf("no verified offline package for %s %s: %w", spec.ComponentType, version, err)
	}
	return pkg.StoragePath, nil
}

const maxCustomPackageBytes int64 = 5 << 30

func (d *BinaryDeployer) downloadCustomPackage(ctx context.Context, spec BinarySpec, version string) (string, error) {
	if !network.Available() {
		return "", fmt.Errorf("offline environment: custom package is not available locally")
	}
	rawURL := strings.ReplaceAll(spec.PackageURL, "{version}", version)
	parsed, err := neturl.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("custom package URL must be an absolute HTTPS URL")
	}
	ext := ".tar.gz"
	lowerPath := strings.ToLower(parsed.Path)
	if strings.HasSuffix(lowerPath, ".zip") {
		ext = ".zip"
	} else if strings.HasSuffix(lowerPath, ".tgz") {
		ext = ".tgz"
	}
	filename := string(spec.ComponentType) + "-" + version + ext
	packageDir := filepath.Join(d.basePath, "packages")
	if err := os.MkdirAll(packageDir, 0755); err != nil {
		return "", fmt.Errorf("create package directory: %w", err)
	}
	storagePath, err := safePackageStoragePath(packageDir, filename)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(storagePath); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		if err := d.registerCustomPackage(spec, version, storagePath, info.Size()); err != nil {
			return "", err
		}
		return storagePath, nil
	}

	client := &http.Client{
		Timeout:   10 * time.Minute,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: utils.SafeDialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "https" || len(via) == 0 || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("custom package redirects must remain on the original HTTPS host")
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download custom package: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download custom package: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxCustomPackageBytes {
		return "", fmt.Errorf("custom package exceeds 5GB limit")
	}
	temp, err := os.CreateTemp(packageDir, "."+filename+".*.downloading")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	written, copyErr := io.Copy(temp, io.LimitReader(response.Body, maxCustomPackageBytes+1))
	closeErr := temp.Close()
	if copyErr != nil {
		return "", fmt.Errorf("download custom package: %w", copyErr)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if written > maxCustomPackageBytes {
		return "", fmt.Errorf("custom package exceeds 5GB limit")
	}
	if err := os.Rename(tempPath, storagePath); err != nil {
		return "", fmt.Errorf("store custom package: %w", err)
	}
	if err := d.registerCustomPackage(spec, version, storagePath, written); err != nil {
		_ = os.Remove(storagePath)
		return "", err
	}
	return storagePath, nil
}

func (d *BinaryDeployer) registerCustomPackage(spec BinarySpec, version, storagePath string, size int64) error {
	if d.db == nil {
		return nil
	}
	checksum, err := calcFileSHA256(storagePath)
	if err != nil {
		return err
	}
	packageRecordMu.Lock()
	defer packageRecordMu.Unlock()
	var existing models.Package
	query := d.db.Where("type = ? AND version = ? AND format = ?", string(spec.ComponentType), version, "binary").First(&existing)
	values := map[string]interface{}{
		"name": spec.BinName + "-" + version, "storage_path": storagePath,
		"size": size, "checksum": checksum, "download_url": spec.PackageURL,
		"description": "Custom component package snapshot download",
	}
	if query.Error == nil {
		return d.db.Model(&existing).Updates(values).Error
	}
	if query.Error != gorm.ErrRecordNotFound {
		return query.Error
	}
	pkg := models.Package{
		Name: spec.BinName + "-" + version, Type: spec.ComponentType, Version: version,
		Format: "binary", Size: size, Checksum: checksum, StoragePath: storagePath,
		DownloadURL: spec.PackageURL, Description: "Custom component package snapshot download",
	}
	return d.db.Create(&pkg).Error
}

const maxExtractedBinaryBytes int64 = 1 << 30

// extractBinaryFromPackage extracts the named binary from a component package
// (.tar.gz or .zip). Only the requested file name is written (path-traversal safe).
func extractBinaryFromPackage(pkgPath, binName, dstPath, format string) error {
	f, err := os.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if format == "zip" {
		zr, err := zip.NewReader(f, fileSize(pkgPath))
		if err != nil {
			return fmt.Errorf("not a zip package: %w", err)
		}
		for pass := 0; pass < 2; pass++ {
			for _, zf := range zr.File {
				if zf.FileInfo().IsDir() {
					continue
				}
				exact, fallback := archiveBinaryCandidate(zf.Name, binName, zf.Mode())
				if (pass == 0 && !exact) || (pass == 1 && !fallback) {
					continue
				}
				if zf.Mode()&os.ModeSymlink != 0 || zf.UncompressedSize64 > uint64(maxExtractedBinaryBytes) {
					return fmt.Errorf("unsafe or oversized binary entry %s", zf.Name)
				}
				rc, err := zf.Open()
				if err != nil {
					return err
				}
				tmp := dstPath + ".tmp"
				out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
				if err != nil {
					rc.Close()
					return err
				}
				written, err := io.Copy(out, io.LimitReader(rc, maxExtractedBinaryBytes+1))
				rc.Close()
				out.Close()
				if err != nil {
					os.Remove(tmp)
					return err
				}
				if written > maxExtractedBinaryBytes {
					os.Remove(tmp)
					return fmt.Errorf("binary %s exceeds extraction limit", binName)
				}
				if err := os.Chmod(tmp, 0755); err != nil {
					os.Remove(tmp)
					return err
				}
				return os.Rename(tmp, dstPath)
			}
		}
		return fmt.Errorf("binary %s not found in package", binName)
	}

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip package: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	fallbackPath := dstPath + ".fallback.tmp"
	_ = os.Remove(fallbackPath)
	defer os.Remove(fallbackPath)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		exact, fallback := archiveBinaryCandidate(hdr.Name, binName, hdr.FileInfo().Mode())
		if !exact && !fallback {
			continue
		}
		if hdr.Size < 0 || hdr.Size > maxExtractedBinaryBytes {
			return fmt.Errorf("binary %s exceeds extraction limit", binName)
		}
		target := dstPath + ".tmp"
		if fallback {
			if _, err := os.Stat(fallbackPath); err == nil {
				continue
			}
			target = fallbackPath
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return err
		}
		written, err := io.Copy(out, io.LimitReader(tr, maxExtractedBinaryBytes+1))
		out.Close()
		if err != nil {
			os.Remove(target)
			return err
		}
		if written > maxExtractedBinaryBytes {
			os.Remove(target)
			return fmt.Errorf("binary %s exceeds extraction limit", binName)
		}
		if err := os.Chmod(target, 0755); err != nil {
			os.Remove(target)
			return err
		}
		if exact {
			return os.Rename(target, dstPath)
		}
	}
	if _, err := os.Stat(fallbackPath); err == nil {
		return os.Rename(fallbackPath, dstPath)
	}
	return fmt.Errorf("binary %s not found in package", binName)
}

func archiveBinaryCandidate(name, binName string, mode os.FileMode) (exact, fallback bool) {
	base := filepath.Base(name)
	if base == binName {
		return true, false
	}
	return false, strings.HasPrefix(base, binName+"-") && filepath.Ext(base) == "" && mode&0111 != 0
}

// extractFullDirFromPackage extracts the whole tree of a FullDir component
// (grafana: bin/, conf/, public/, ...) into dstDir, stripping the outer
// top-level directory from the archive. Path traversal is rejected.
func extractFullDirFromPackage(pkgPath, dstDir string, spec BinarySpec) error {
	f, err := os.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}

	var stripPrefix string
	var extracted int64
	if spec.ArchiveFormat == "zip" {
		zr, err := zip.NewReader(f, fileSize(pkgPath))
		if err != nil {
			return fmt.Errorf("not a zip package: %w", err)
		}
		for _, zf := range zr.File {
			name, err := cleanArchiveEntryName(zf.Name)
			if err != nil {
				return err
			}
			if name == "" {
				continue
			}
			if stripPrefix == "" {
				stripPrefix = firstPathSegment(name)
			}
			rel, err := stripArchivePrefix(name, stripPrefix)
			if err != nil {
				return err
			}
			if rel == "" {
				continue
			}
			dst, err := archiveDestination(dstDir, rel)
			if err != nil {
				return err
			}
			mode := zf.Mode()
			if mode&os.ModeSymlink != 0 {
				return fmt.Errorf("symbolic link blocked: %s", zf.Name)
			}
			if zf.FileInfo().IsDir() {
				if err := os.MkdirAll(dst, 0755); err != nil {
					return err
				}
				continue
			}
			if mode&os.ModeType != 0 {
				return fmt.Errorf("unsupported archive entry: %s", zf.Name)
			}
			if zf.UncompressedSize64 > uint64(maxExtractedTreeBytes) ||
				extracted > maxExtractedTreeBytes-int64(zf.UncompressedSize64) {
				return fmt.Errorf("archive exceeds extraction limit")
			}
			extracted += int64(zf.UncompressedSize64)
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
			if err != nil {
				rc.Close()
				return err
			}
			written, copyErr := io.Copy(out, io.LimitReader(rc, int64(zf.UncompressedSize64)+1))
			rc.Close()
			out.Close()
			if copyErr != nil {
				return copyErr
			}
			if written != int64(zf.UncompressedSize64) {
				return fmt.Errorf("archive entry size mismatch: %s", zf.Name)
			}
		}
		return nil
	}

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip package: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name, err := cleanArchiveEntryName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if stripPrefix == "" {
			stripPrefix = firstPathSegment(name)
		}
		rel, err := stripArchivePrefix(name, stripPrefix)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		dst, err := archiveDestination(dstDir, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || hdr.Size > maxExtractedTreeBytes || extracted > maxExtractedTreeBytes-hdr.Size {
				return fmt.Errorf("archive exceeds extraction limit")
			}
			extracted += hdr.Size
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return err
			}
			written, copyErr := io.Copy(out, io.LimitReader(tr, hdr.Size+1))
			out.Close()
			if copyErr != nil {
				return copyErr
			}
			if written != hdr.Size {
				return fmt.Errorf("archive entry size mismatch: %s", hdr.Name)
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("link entry blocked: %s", hdr.Name)
		}
	}
	return nil
}

const maxExtractedTreeBytes int64 = 10 << 30

func cleanArchiveEntryName(raw string) (string, error) {
	name := filepath.ToSlash(raw)
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return "", nil
	}
	cleaned := path.Clean(name)
	if strings.HasPrefix(name, "/") || cleaned != name || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("path traversal blocked: %s", raw)
	}
	return cleaned, nil
}

func stripArchivePrefix(name, prefix string) (string, error) {
	if name == prefix {
		return "", nil
	}
	if !strings.HasPrefix(name, prefix+"/") {
		return "", fmt.Errorf("archive entry is outside top-level directory: %s", name)
	}
	return strings.TrimPrefix(name, prefix+"/"), nil
}

func archiveDestination(root, relative string) (string, error) {
	destination := filepath.Join(root, filepath.FromSlash(relative))
	if !utils.PathWithinBase(root, destination) {
		return "", fmt.Errorf("path traversal blocked: %s", relative)
	}
	return destination, nil
}

func firstPathSegment(p string) string {
	p = strings.TrimPrefix(p, "./")
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
