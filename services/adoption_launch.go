package services

import (
	"github.com/promlifecycle/platform/models"
	"strings"
)

// ValidateAdoptedServiceLaunch reuses the strict typed systemd parser so that
// the next restart executes the same binary and argv as the discovered process.
// The argv and D-Bus response can contain secrets and must not be logged.
func ValidateAdoptedServiceLaunch(raw, binary string, args []string) error {
	argv := append([]string{binary}, args...)
	_, err := parseRecoveryPointBusLaunch(raw, &models.Component{BinaryPath: binary}, pointDigest([]byte(strings.Join(argv, "\x00")+"\x00")))
	return err
}
