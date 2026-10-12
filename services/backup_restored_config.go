package services

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/promlifecycle/platform/core/deployer"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const restoredConfigMaxBytes = 4 << 20

// Prepared from a verified restore source while the component claim is held.
// This is not a new source of authority: publication still verifies live bytes.
type restoredConfigActivation struct {
	Present                                      bool
	ComponentID                                  uint
	Name, FilePath, Content, ContentType, Source string
}

// prepareRestoredConfigs reads only names already managed by configuration
// publication. A component without history retains its existing first-publish
// behavior, which captures the live configuration as its baseline.
func (s *BackupService) prepareRestoredConfigs(ctx context.Context, r hostCommandRunner, c *models.Component, artifact string, manifest *models.BackupManifest, source string) ([]restoredConfigActivation, error) {
	if manifest.Scope == "data" {
		return nil, nil
	}
	if (manifest.Scope != "config" && manifest.Scope != "full") || len(manifest.SourcePaths) == 0 || manifest.SourcePaths[0] != c.ConfigPath {
		return nil, errors.New("restored configuration source does not match component")
	}
	var history []models.ComponentConfig
	if err := s.db.WithContext(ctx).Where("component_id = ?", c.ID).Order("version DESC, id DESC").Find(&history).Error; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := make([]restoredConfigActivation, 0)
	for _, record := range history {
		if seen[record.Name] {
			continue
		}
		seen[record.Name] = true
		if !utils.SafePathSegment(record.Name) {
			return nil, errors.New("invalid managed configuration name")
		}
		target, err := componentLiveConfigPath(c, record.Name)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(target) || filepath.Clean(target) != target || !filepath.IsAbs(c.ConfigPath) || filepath.Clean(c.ConfigPath) != c.ConfigPath {
			return nil, errors.New("invalid restored configuration path")
		}
		relative, err := filepath.Rel(c.ConfigPath, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, "../") || filepath.IsAbs(relative) {
			return nil, errors.New("managed configuration lies outside restore source")
		}
		var command string
		switch manifest.Scope {
		case "config":
			path := artifact
			if manifest.Format == "directory" {
				if relative == "." {
					return nil, errors.New("managed configuration does not identify a restored file")
				}
				path = filepath.Join(artifact, relative)
			} else if manifest.Format != "file" || relative != "." {
				return nil, errors.New("restored configuration format/path mismatch")
			}
			q := utils.QuoteShell(path)
			command = "if [ ! -e " + q + " ] && [ ! -L " + q + " ]; then printf 0; else test -f " + q + " && test ! -L " + q + " && printf 1 && head -c 4194305 -- " + q + "; fi"
		case "full":
			member := "./config/" + filepath.Base(c.ConfigPath)
			if relative != "." {
				member += "/" + filepath.ToSlash(relative)
			}
			// pipefail retains archive/read failures, and the bounded output avoids
			// loading an oversized managed configuration into platform memory.
			archive, entry := utils.QuoteShell(artifact), utils.QuoteShell(member)
			script := "if entries=$(tar -tzf " + archive + " -- " + entry + " 2>/dev/null); then test \"$entries\" = " + entry + " && printf 1 && tar -xOzf " + archive + " -- " + entry + " | head -c 4194305; else printf 0; fi"
			command = "bash -o pipefail -c " + utils.QuoteShell(script)
		}
		content, err := r.Run(ctx, command)
		if err != nil {
			return nil, restoredConfigReadError("read restored configuration source", err)
		}
		if content == "" || content[0] != '0' && content[0] != '1' || content[0] == '0' && len(content) != 1 {
			return nil, errors.New("invalid restored configuration presence evidence")
		}
		present := content[0] == '1'
		content = content[1:]
		if len(content) > restoredConfigMaxBytes {
			return nil, errors.New("restored managed configuration exceeds 4 MiB")
		}
		result = append(result, restoredConfigActivation{Present: present, ComponentID: c.ID, Name: record.Name, FilePath: target, Content: content, ContentType: record.ContentType, Source: source})
	}
	if len(result) > 0 {
		digest, size, err := inspectArtifact(ctx, r, artifact, manifest.Scope, manifest.Format, manifest.SourcePaths)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(digest, manifest.SHA256) || size != manifest.Size {
			return nil, errors.New("restored configuration source integrity changed")
		}
	}
	return result, nil
}

func verifyRestoredConfigs(ctx context.Context, r hostCommandRunner, configs []restoredConfigActivation) error {
	for _, cfg := range configs {
		q := utils.QuoteShell(cfg.FilePath)
		if !cfg.Present {
			if _, err := r.Run(ctx, "test ! -e "+q+" && test ! -L "+q); err != nil {
				return restoredConfigReadError("verify restored configuration absence", err)
			}
			continue
		}
		content, err := r.Run(ctx, "test -f "+q+" && test ! -L "+q+" && head -c 4194305 -- "+q)
		if err != nil {
			return restoredConfigReadError("verify restored configuration", err)
		}
		if content != cfg.Content {
			return errors.New("restored live configuration differs from verified source")
		}
	}
	return nil
}

// Called only inside the same transaction that publishes the successful
// restore outcome. Historical rows remain intact, including nonmatching rows.
func activateRestoredConfigs(tx *gorm.DB, configs []restoredConfigActivation) error {
	tx = tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	for _, cfg := range configs {
		var history []models.ComponentConfig
		if err := tx.Where("component_id = ? AND name = ?", cfg.ComponentID, cfg.Name).Order("version DESC, id DESC").Find(&history).Error; err != nil {
			return err
		}
		var matching models.ComponentConfig
		maxVersion := 0
		for _, record := range history {
			if record.Version > maxVersion {
				maxVersion = record.Version
			}
			// Keep raw configuration (which may contain credentials) out of
			// SQL predicates and database error logs.
			if matching.ID == 0 && record.FilePath == cfg.FilePath && record.Content == cfg.Content {
				matching = record
			}
		}
		if err := tx.Model(&models.ComponentConfig{}).Where("component_id = ? AND name = ?", cfg.ComponentID, cfg.Name).Update("is_active", false).Error; err != nil {
			return err
		}
		if !cfg.Present {
			// A directory backup may predate this managed file. Preserve its
			// history while marking the restored absence accurately.
			continue
		}
		if matching.ID != 0 {
			result := tx.Model(&models.ComponentConfig{}).Where("id = ? AND component_id = ? AND name = ? AND file_path = ? AND version = ?", matching.ID, cfg.ComponentID, cfg.Name, cfg.FilePath, matching.Version).Update("is_active", true)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("restored configuration history changed")
			}
		} else {
			// Valid backups can predate managed configuration history. The verified
			// artifact, rather than the stale active row, supplies the new version.
			record := models.ComponentConfig{ComponentID: cfg.ComponentID, Name: cfg.Name, Content: cfg.Content, ContentType: cfg.ContentType, FilePath: cfg.FilePath, Version: maxVersion + 1, IsActive: true, Description: "Restored from " + cfg.Source}
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// A runner error can contain partial configuration stdout. Retain only stable
// classes used by recovery decisions, never the original error/output chain.
func restoredConfigReadError(phase string, cause error) error {
	safe := []error{errors.New(phase + " failed")}
	for _, class := range []error{
		coressh.ErrRemoteTerminationUnconfirmed, deployer.ErrBinaryRecoveryUnconfirmed,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(cause, class) {
			safe = append(safe, class)
		}
	}
	return errors.Join(safe...)
}
