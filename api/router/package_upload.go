package router

import (
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// packageUploadLimits is mounted only after authentication and system-admin
// authorization on the package upload route. Other API requests retain the
// server's normal deadlines, including subsequent requests on this connection.
func packageUploadLimits(maxBodyBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxBodyBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "上传请求体积过大，文件最大允许 5GB"})
			return
		}
		controller := http.NewResponseController(c.Writer)
		started := time.Now()
		if err := controller.SetWriteDeadline(started.Add(330 * time.Second)); err != nil {
			log.Printf("[Packages] Cannot set upload write deadline: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "无法设置上传响应时限，请联系管理员"})
			return
		}
		if err := controller.SetReadDeadline(started.Add(300 * time.Second)); err != nil {
			log.Printf("[Packages] Cannot set upload read deadline: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "无法设置上传读取时限，请联系管理员"})
			return
		}

		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		defer func() {
			if form := c.Request.MultipartForm; form != nil {
				_ = form.RemoveAll()
			}
		}()
		// Parse once under the upload limits. SingleNodeScope and UploadImage reuse
		// this form; parse errors must not be swallowed by the generic scope parser.
		if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
			var oversized *http.MaxBytesError
			var networkError net.Error
			switch {
			case errors.As(err, &oversized):
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "上传请求体积过大，文件最大允许 5GB"})
			case errors.As(err, &networkError) && networkError.Timeout():
				c.AbortWithStatusJSON(http.StatusRequestTimeout, gin.H{"error": "上传读取超时，请检查网络后重试"})
			default:
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "读取上传表单失败，请重新选择文件"})
			}
			return
		}
		c.Next()
	}
}
