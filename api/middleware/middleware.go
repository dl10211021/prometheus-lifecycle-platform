package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

// jwtSecret is shared with pkg/credentials (encrypted host credentials derive
// their key from the same secret file), so both stay consistent across
// restarts and survive secret rotation together.
var jwtSecret = func() string {
	s, ok := credentials.LoadJWTSecret()
	if !ok {
		log.Printf("[JWT] WARNING: cannot persist JWT secret, using random secret - all tokens and encrypted credentials will be invalidated on restart")
	}
	return s
}()

var loginRateLimiter = struct {
	sync.Mutex
	// map[ip -> list of attempt times]
	attempts map[string][]time.Time
}{
	attempts: make(map[string][]time.Time),
}

const maxLoginAttempts = 20
const rateLimitWindow = 60 * time.Second

func init() {
	// Clean up stale rate limiter entries every 60s
	go func() {
		ticker := time.NewTicker(rateLimitWindow)
		defer ticker.Stop()
		for range ticker.C {
			loginRateLimiter.Lock()
			now := time.Now()
			for ip, times := range loginRateLimiter.attempts {
				if len(times) == 0 || now.Sub(times[0]) > rateLimitWindow {
					delete(loginRateLimiter.attempts, ip)
				}
			}
			loginRateLimiter.Unlock()
		}
	}()
}

func isRateLimited(ip string) bool {
	loginRateLimiter.Lock()
	defer loginRateLimiter.Unlock()

	now := time.Now()
	cutoff := now.Add(-rateLimitWindow)

	var valid []time.Time
	for _, t := range loginRateLimiter.attempts[ip] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= maxLoginAttempts {
		loginRateLimiter.attempts[ip] = valid
		return true
	}

	loginRateLimiter.attempts[ip] = append(valid, now)
	return false
}

type Claims struct {
	UserID         uint   `json:"user_id"`
	Username       string `json:"username"`
	Role           string `json:"role"`
	SessionVersion int64  `json:"session_version"`
	jwt.RegisteredClaims
}

func Login(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if isRateLimited(ip) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录尝试过于频繁，请稍后再试"})
			return
		}

		var req struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
			return
		}
		var user models.User
		if err := db.First(&user, "username = ?", req.Username).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
			return
		}
		if !utils.CheckPassword(req.Password, user.Password) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
			return
		}
		if !user.IsActive {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "账号已停用，请联系管理员"})
			return
		}
		if !models.IsValidUserRole(user.Role) {
			c.JSON(http.StatusForbidden, gin.H{"error": "账号角色无效，请联系系统管理员"})
			return
		}
		claims := &Claims{
			UserID:         user.ID,
			Username:       user.Username,
			Role:           user.Role,
			SessionVersion: user.SessionVersion,
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			},
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		tokenStr, _ := token.SignedString([]byte(jwtSecret))
		c.Set("user_id", user.ID)
		c.Set("username", user.Username)
		c.Set("role", user.Role)
		c.JSON(http.StatusOK, gin.H{"token": tokenStr, "username": user.Username, "role": user.Role, "must_change_password": user.MustChangePassword})
	}
}

func AuthRequired(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少登录 token，请重新登录"})
			c.Abort()
			return
		}
		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		if tokenStr == "" || len(tokenStr) < 10 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "登录 token 无效或已过期，请重新登录"})
			c.Abort()
			return
		}
		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			return []byte(jwtSecret), nil
		}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "登录 token 无效或已过期，请重新登录"})
			c.Abort()
			return
		}
		var user models.User
		if err := db.First(&user, claims.UserID).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "用户不存在，请重新登录"})
			c.Abort()
			return
		}
		if claims.SessionVersion != user.SessionVersion {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "密码已更新，请重新登录"})
			c.Abort()
			return
		}
		if !user.IsActive {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "账号已停用，请联系管理员"})
			c.Abort()
			return
		}
		if !models.IsValidUserRole(user.Role) {
			c.JSON(http.StatusForbidden, gin.H{"error": "账号角色无效，请联系系统管理员"})
			c.Abort()
			return
		}
		c.Set("user_id", user.ID)
		c.Set("username", user.Username)
		c.Set("role", user.Role)
		c.Set("must_change_password", user.MustChangePassword)
		c.Next()
	}
}

func RequireRole(allowedRoles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get("role")
		roleStr, ok := role.(string)
		if !ok || roleStr == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "缺少用户角色，无法执行此操作"})
			c.Abort()
			return
		}
		for _, r := range allowedRoles {
			if roleStr == r {
				c.Next()
				return
			}
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "权限不足，无法执行此操作"})
		c.Abort()
	}
}

func Me(c *gin.Context) {
	userID, _ := c.Get("user_id")
	mustChangePassword, _ := c.Get("must_change_password")
	c.JSON(http.StatusOK, gin.H{
		"id":                   userID,
		"user_id":              userID,
		"username":             c.GetString("username"),
		"role":                 c.GetString("role"),
		"must_change_password": mustChangePassword,
	})
}

// ChangePassword lets an authenticated user replace their own password and
// clears the one-time-password requirement.
func ChangePassword(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			CurrentPassword string `json:"current_password" binding:"required"`
			NewPassword     string `json:"new_password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
			return
		}
		userID, ok := c.Get("user_id")
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "用户不存在，请重新登录"})
			return
		}
		var user models.User
		if err := db.First(&user, userID).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "用户不存在，请重新登录"})
			return
		}
		if !utils.CheckPassword(req.CurrentPassword, user.Password) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "当前密码不正确"})
			return
		}
		if !utils.ValidPasswordLength(req.NewPassword) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "密码必须为 1–72 UTF-8 字节"})
			return
		}
		hashed := utils.HashPassword(req.NewPassword)
		if hashed == "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "处理新密码失败"})
			return
		}
		result := db.Model(&user).Where("session_version = ? AND password = ?", user.SessionVersion, user.Password).Updates(map[string]interface{}{
			"password":             hashed,
			"must_change_password": false,
			"session_version":      gorm.Expr("session_version + 1"),
		})
		if result.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "更新密码失败"})
			return
		}
		if result.RowsAffected != 1 {
			c.JSON(http.StatusConflict, gin.H{"error": "密码已被其他请求更新，请重新登录"})
			return
		}
		c.Set("must_change_password", false)
		c.JSON(http.StatusOK, gin.H{"message": "密码已更新，请重新登录", "must_change_password": false, "require_relogin": true})
	}
}

const maxSingleNodeJSONBodyBytes int64 = 4 << 20

// SingleNodeScope injects the hidden singleton ID into legacy handler inputs.
// Public callers never need to know or submit a cluster_id.
func SingleNodeScope(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		nodeID, err := services.SingleNodeID(db)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "节点范围不可用，请稍后重试"})
			return
		}
		id := strconv.FormatUint(uint64(nodeID), 10)
		c.Set("single_node_id", nodeID)
		if c.Param("cluster_id") == "" {
			c.Params = append(c.Params, gin.Param{Key: "cluster_id", Value: id})
		}
		query := c.Request.URL.Query()
		query.Set("cluster_id", id)
		c.Request.URL.RawQuery = query.Encode()

		contentType := c.ContentType()
		if contentType == "application/json" && c.Request.Body != nil {
			limitedBody := http.MaxBytesReader(c.Writer, c.Request.Body, maxSingleNodeJSONBodyBytes)
			body, readErr := io.ReadAll(limitedBody)
			_ = limitedBody.Close()
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			if readErr != nil {
				var maxBytesErr *http.MaxBytesError
				if errors.As(readErr, &maxBytesErr) {
					c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "JSON 请求内容超过 4 MiB，请缩小后重试"})
				} else {
					c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "读取请求内容失败"})
				}
				return
			}
			if len(bytes.TrimSpace(body)) > 0 {
				var object map[string]interface{}
				if json.Unmarshal(body, &object) == nil && object != nil {
					object["cluster_id"] = nodeID
					body, err = json.Marshal(object)
					if err != nil {
						c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "编码请求内容失败"})
						return
					}
				}
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
		} else if strings.HasPrefix(contentType, "multipart/form-data") {
			if err := c.Request.ParseMultipartForm(32 << 20); err == nil {
				c.Request.PostForm.Set("cluster_id", id)
			}
		} else if contentType == "application/x-www-form-urlencoded" {
			if err := c.Request.ParseForm(); err == nil {
				c.Request.PostForm.Set("cluster_id", id)
			}
		}
		c.Next()
	}
}

func CORS() gin.HandlerFunc {
	allowed := make(map[string]struct{})
	for _, origin := range strings.Split(os.Getenv("PROMLIFECYCLE_ALLOWED_ORIGINS"), ",") {
		origin = strings.TrimSpace(origin)
		if origin != "" && origin != "*" {
			allowed[origin] = struct{}{}
		}
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		_, originAllowed := allowed[origin]
		if origin != "" {
			c.Header("Vary", "Origin")
		}
		if originAllowed {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		}
		if c.Request.Method == http.MethodOptions {
			if origin == "" || !originAllowed {
				c.AbortWithStatus(http.StatusForbidden)
			} else {
				c.AbortWithStatus(http.StatusNoContent)
			}
			return
		}
		c.Next()
	}
}
