package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

// ===== Streaming Logs =====

// StreamLogs streams container logs from the component's actual Docker host.
func (h *OpsHandler) StreamLogs(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var component models.Component
	if h.db.Preload("Host").First(&component, id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "组件不存在"})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	containerName := component.ServiceName
	if containerName == "" {
		containerName = component.Name
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()

	var reader io.Reader
	var waitCommand func()
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", "200", "--follow", containerName)
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			writeSSEError(c, "获取日志管道失败: "+err.Error())
			return
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			writeSSEError(c, "启动 docker logs 失败: "+err.Error())
			return
		}
		reader = pipe
		waitCommand = func() { _ = cmd.Wait() }
	} else {
		client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username,
			component.Host.Password, component.Host.PrivateKey)
		if err != nil {
			writeSSEError(c, "SSH 连接远程 Docker 主机失败: "+err.Error())
			return
		}
		defer client.Close()
		session, err := client.NewSession()
		if err != nil {
			writeSSEError(c, "创建远程日志会话失败: "+err.Error())
			return
		}
		defer session.Close()
		pipe, err := session.StdoutPipe()
		if err != nil {
			writeSSEError(c, "获取远程日志管道失败: "+err.Error())
			return
		}
		command := "docker logs --tail 200 --follow " + utils.QuoteShell(containerName) + " 2>&1"
		if err := session.Start(command); err != nil {
			writeSSEError(c, "启动远程 docker logs 失败: "+err.Error())
			return
		}
		reader = pipe
		waitCommand = func() { _ = session.Wait() }
		go func() {
			<-ctx.Done()
			_ = session.Close()
		}()
	}

	buffer := make([]byte, 4096)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			payload, _ := json.Marshal(gin.H{
				"timestamp": time.Now().Format(time.RFC3339),
				"log":       strings.TrimRight(string(buffer[:n]), "\n"),
			})
			c.Writer.WriteString(fmt.Sprintf("data: %s\n\n", payload))
			c.Writer.Flush()
		}
		if readErr != nil {
			break
		}
	}
	waitCommand()
}

func writeSSEError(c *gin.Context, message string) {
	payload, _ := json.Marshal(gin.H{"error": message})
	c.Writer.WriteString(fmt.Sprintf("data: %s\n\n", payload))
	c.Writer.Flush()
}
