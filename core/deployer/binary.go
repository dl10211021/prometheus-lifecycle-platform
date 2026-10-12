package deployer

import "gorm.io/gorm"

// BinaryDeployer deploys single-binary agents/servers to hosts WITHOUT Docker,
// via SSH: extract the binary from an uploaded offline package (preferred) or from
// the official image, upload it, and manage it with a systemd unit.
type BinaryDeployer struct {
	db       *gorm.DB
	basePath string
	progress ProgressReporter
}

func NewBinaryDeployer(db *gorm.DB, basePath string) *BinaryDeployer {
	if basePath == "" {
		basePath = "/opt/promlifecycle"
	}
	return &BinaryDeployer{db: db, basePath: basePath}
}

func (d *BinaryDeployer) WithProgress(reporter ProgressReporter) *BinaryDeployer {
	d.progress = reporter
	return d
}
