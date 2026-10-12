package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

var ErrManualPrometheusConfig = errors.New("prometheus.yml is manually managed; automatic monitoring publication is blocked until manual ownership is released")

// WithMonitoringLock serializes desired-state mutations with snapshot generation,
// publication and acknowledgement. It is deliberately shared across handlers.
func WithMonitoringLock(fn func() error) error {
	return WithConfigLock(0, "monitoring-desired-state", fn)
}

func monitoringModels() []interface{} {
	return []interface{}{&models.AlertRule{}, &models.RecordingRule{}, &models.ScrapeConfigModel{}, &models.RemoteWriteModel{}}
}

func finishPublication(db *gorm.DB, clusterID uint, records []interface{}, publicationErr error) error {
	status, message := "applied", ""
	if publicationErr != nil {
		status, message = "error", publicationErr.Error()
	}
	if errors.Is(publicationErr, ErrManualPrometheusConfig) {
		status = "manual"
	}
	if errors.Is(publicationErr, errDashboardAwaitingApply) {
		status = "pending"
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range records {
			changes := map[string]interface{}{"apply_status": status, "last_error": message}
			if publicationErr == nil {
				changes["applied_at"] = time.Now().UTC()
			}
			if err := tx.Model(model).Where("cluster_id = ?", clusterID).Updates(changes).Error; err != nil {
				return err
			}
			if publicationErr == nil {
				if err := tx.Where("cluster_id = ? AND delete_pending = ?", clusterID, true).Delete(model).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	return errors.Join(publicationErr, err)
}

// publishMonitoringBundle validates every candidate before replacing any file.
// The durable receipt is written only after one acknowledged reload; unchanged
// candidates skip publication only if every live file still matches that receipt.
func (ac *AutoConfig) publishMonitoringBundle(clusterID uint, comp *models.Component, configPath string, configData []byte) error {
	var alerts []models.AlertRule
	if err := ac.db.Where("cluster_id = ? AND enabled = ?", clusterID, true).Order("id").Find(&alerts).Error; err != nil {
		return err
	}
	var recordings []models.RecordingRule
	if err := ac.db.Where("cluster_id = ? AND enabled = ?", clusterID, true).Order("id").Find(&recordings).Error; err != nil {
		return err
	}
	alertData, err := buildAlertRulesYAML(alerts)
	if err != nil {
		return err
	}
	recordingData, err := buildRecordingRulesYAML(recordings)
	if err != nil {
		return err
	}
	files := map[string][]byte{"prometheus.yml": configData, "rules/rules.yml": []byte(alertData), "rules/recording.yml": []byte(recordingData)}
	stage, err := os.MkdirTemp(filepath.Dir(configPath), ".monitoring-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	names := []string{"prometheus.yml", "rules/recording.yml", "rules/rules.yml"}
	digest := sha256.New()
	fmt.Fprintf(digest, "%d\x00%s\x00%s\x00%s\x00%s\x00", comp.ID, comp.Host.IP, comp.ConfigPath, comp.ExternalURL, comp.InternalURL)
	for _, name := range names {
		path := filepath.Join(stage, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := utils.AtomicWriteFile(path, files[name], 0600); err != nil {
			return err
		}
		digest.Write([]byte(name))
		digest.Write(files[name])
	}
	// Validate the main config against the staged rule set, not yesterday's live
	// rules, and only after every candidate file has been materialized.
	var candidate PrometheusConfig
	if err := yaml.Unmarshal(configData, &candidate); err != nil {
		return err
	}
	candidate.RuleFiles = []string{filepath.Join(stage, "rules", "*.yml")}
	validationData, err := yaml.Marshal(candidate)
	if err != nil {
		return err
	}
	if err := utils.AtomicWriteFile(filepath.Join(stage, "prometheus.yml"), validationData, 0600); err != nil {
		return err
	}
	for _, name := range names {
		kind := "prometheus-rules"
		if name == "prometheus.yml" {
			kind = "prometheus"
		}
		if _, err := ValidateConfigArtifact(kind, filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	receipt := []byte(fmt.Sprintf("%x", digest.Sum(nil)))
	receiptPath := configPath + ".applied.sha256"
	// Any interrupted or failed publication leaves no receipt, forcing a reload.
	priorReceipt, _ := os.ReadFile(receiptPath)
	root := filepath.Dir(configPath)
	if comp.DeployType == "binary" {
		root = filepath.Dir(comp.ConfigPath)
	}
	livePath := func(name string) string {
		if name == "prometheus.yml" && comp.DeployType == "binary" {
			return comp.ConfigPath
		}
		return filepath.Join(root, name)
	}
	var client *ssh.Client
	if comp.DeployType == "binary" && !ssh.IsLocalHost(comp.Host.IP, comp.Host.ConnectionType) {
		client, err = ssh.NewClient(comp.Host.IP, comp.Host.Port, comp.Host.Username, comp.Host.Password, comp.Host.PrivateKey)
		if err != nil {
			return err
		}
		defer client.Close()
	}
	readLive := func(path string) ([]byte, bool, error) {
		if client == nil {
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				return nil, false, nil
			}
			return data, err == nil, err
		}
		out, _, err := client.RunCommand("if [ -f " + utils.QuoteShell(path) + " ]; then printf 'present\\n'; cat " + utils.QuoteShell(path) + "; else printf 'absent\\n'; fi")
		if err != nil {
			return nil, false, err
		}
		if strings.HasPrefix(out, "present\n") {
			return []byte(strings.TrimPrefix(out, "present\n")), true, nil
		}
		if out == "absent\n" || out == "absent" {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("invalid file read response")
	}
	writeLive := func(path string, data []byte, exists bool) error {
		if client == nil {
			if !exists {
				err := os.Remove(path)
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			return utils.AtomicWriteFile(path, data, 0644)
		}
		if !exists {
			_, _, err := client.RunCommand("rm -f " + utils.QuoteShell(path))
			return err
		}
		if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(filepath.Dir(path))); err != nil {
			return err
		}
		return client.CopyFile(bytes.NewReader(data), path, 0644, int64(len(data)))
	}
	old := map[string][]byte{}
	existed := map[string]bool{}
	unchanged := bytes.Equal(priorReceipt, receipt)
	for _, name := range names {
		data, exists, err := readLive(livePath(name))
		if err != nil {
			return err
		}
		old[name], existed[name] = data, exists
		unchanged = unchanged && exists && bytes.Equal(data, files[name])
	}
	if unchanged {
		return nil
	}
	if err := os.Remove(receiptPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	restore := func() error {
		var errs []error
		for _, name := range names {
			errs = append(errs, writeLive(livePath(name), old[name], existed[name]))
		}
		return errors.Join(errs...)
	}
	for _, name := range names {
		if err := writeLive(livePath(name), files[name], true); err != nil {
			return errors.Join(err, restore())
		}
	}
	if err := ReloadPrometheusComponent(comp); err != nil {
		restoreErr := restore()
		if restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore failed: %w", restoreErr))
		}
		return errors.Join(err, ReloadPrometheusComponent(comp))
	}
	// Keep the platform copy useful for inspection, without overwriting a live
	// destination twice when both paths identify the same directory.
	if root != filepath.Dir(configPath) {
		for _, name := range names {
			path := filepath.Join(filepath.Dir(configPath), name)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			if err := utils.AtomicWriteFile(path, files[name], 0600); err != nil {
				return err
			}
		}
	}
	return utils.AtomicWriteFile(receiptPath, receipt, 0600)
}
