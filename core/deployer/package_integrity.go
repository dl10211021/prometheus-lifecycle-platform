package deployer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

// ValidatePackage checks the stored bytes, not merely the catalog record. Legacy
// unversioned official Loki archives cannot establish which release they contain.
func ValidatePackage(basePath string, pkg models.Package) error {
	base := filepath.Join(basePath, "packages")
	resolved, err := filepath.EvalSymlinks(pkg.StoragePath)
	if err != nil {
		return err
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	if !utils.PathWithinBase(resolvedBase, resolved) {
		return fmt.Errorf("package file escapes storage directory")
	}
	info, err := os.Lstat(pkg.StoragePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("package is not a nonempty regular file")
	}
	if pkg.Size > 0 && pkg.Size != info.Size() {
		return fmt.Errorf("package size mismatch")
	}
	name := filepath.Base(pkg.StoragePath)
	if name == "loki-linux-amd64.zip" || name == "promtail-linux-amd64.zip" {
		return fmt.Errorf("unversioned legacy archive must be reacquired with an explicit version")
	}
	if pkg.Checksum != "" {
		sum, err := calcFileSHA256(pkg.StoragePath)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sum, pkg.Checksum) {
			return fmt.Errorf("package SHA-256 mismatch")
		}
	}
	return validatePackageArchive(pkg.StoragePath, string(pkg.Type), pkg.Format)
}

// FindAvailablePackage returns only an integrity-checked local package.
func FindAvailablePackage(db *gorm.DB, basePath, compType, version string) (models.Package, error) {
	if db == nil {
		return models.Package{}, gorm.ErrRecordNotFound
	}
	var packages []models.Package
	normalized := strings.TrimPrefix(version, "v")
	if err := db.Where("type = ? AND version IN ? AND format IN ?", compType, []string{normalized, "v" + normalized}, []string{"binary", "zip", "msi"}).Order("id DESC").Find(&packages).Error; err != nil {
		return models.Package{}, err
	}
	for _, pkg := range packages {
		if ValidatePackage(basePath, pkg) == nil {
			return pkg, nil
		}
	}
	return models.Package{}, gorm.ErrRecordNotFound
}

func officialStorageName(compType, version, upstreamName string) string {
	if compType == "loki" || compType == "promtail" {
		return compType + "-" + strings.TrimPrefix(version, "v") + ".linux-amd64.zip"
	}
	return upstreamName
}

func validatePackageArchive(path, compType, format string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var magic [8]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return err
	}
	if format == "msi" {
		if !bytes.Equal(magic[:], []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) {
			return fmt.Errorf("invalid MSI archive")
		}
		return nil
	}
	found := false
	matches := func(name string) bool {
		if _, official := BuiltinVersions[compType]; !official {
			return true
		}
		name = filepath.Base(name)
		return name == compType || ((compType == "loki" || compType == "promtail") && name == compType+"-linux-amd64") || (compType == "grafana" && name == "grafana-server")
	}
	if bytes.Equal(magic[:4], []byte{'P', 'K', 3, 4}) {
		archive, err := zip.OpenReader(path)
		if err != nil {
			return err
		}
		defer archive.Close()
		for _, entry := range archive.File {
			if entry.FileInfo().IsDir() {
				continue
			}
			r, err := entry.Open()
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(io.Discard, r)
			closeErr := r.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if entry.Mode().IsRegular() && n > 0 && matches(entry.Name) {
				found = true
			}
		}
	} else {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("invalid package archive: %w", err)
		}
		defer gz.Close()
		archive := tar.NewReader(gz)
		for {
			entry, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if _, err := io.Copy(io.Discard, archive); err != nil {
				return err
			}
			if entry.Typeflag == tar.TypeReg && entry.Size > 0 && matches(entry.Name) {
				found = true
			}
		}
		if _, err := io.Copy(io.Discard, gz); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("archive contains no %s binary", compType)
	}
	return nil
}
