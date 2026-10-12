package handlers

import (
	"os"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

// GetHostMonitoringStatus performs a read-only, instance-scoped live check.
func (h *Handler) GetHostMonitoringStatus(c *gin.Context) {
	h.getHostMonitoringStatus(c)
}

func readComponentFile(host models.Host, path string) ([]byte, error) {
	if ssh.IsLocalHost(host.IP, host.ConnectionType) {
		return os.ReadFile(path)
	}
	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	output, _, err := client.RunCommand("cat " + utils.QuoteShell(path))
	return []byte(output), err
}
