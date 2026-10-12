package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

func (h *Handler) ListUsers(c *gin.Context) {
	var users []models.User
	h.db.Select("id", "username", "email", "role", "is_active", "created_at", "updated_at").Find(&users)
	c.JSON(http.StatusOK, gin.H{"users": users})
}

func (h *Handler) GetUser(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var user models.User
	if err := h.db.Select("id", "username", "email", "role", "is_active", "created_at", "updated_at").First(&user, id).Error; err != nil {
		utils.HandleNotFoundError(c, "user")
		return
	}
	c.JSON(http.StatusOK, user)
}

func (h *Handler) CreateUser(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
		Email    string `json:"email"`
		Role     string `json:"role"`
		IsActive *bool  `json:"is_active"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	if req.Role == "" {
		req.Role = "viewer"
	}
	if !models.IsValidUserRole(req.Role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "角色无效，仅支持 viewer、operator、change_admin、system_admin 或 auditor"})
		return
	}

	if !utils.ValidPasswordLength(req.Password) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "密码必须为 1–72 UTF-8 字节"})
		return
	}
	hashed := utils.HashPassword(req.Password)
	if hashed == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "密码加密失败"})
		return
	}

	user := models.User{
		Username: req.Username,
		Password: hashed,
		Email:    req.Email,
		Role:     req.Role,
		IsActive: true,
	}
	if req.IsActive != nil {
		user.IsActive = *req.IsActive
	}

	if err := h.db.Create(&user).Error; err != nil {
		if utils.IsDuplicateEntryError(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "用户名已存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建用户失败"})
		return
	}

	h.db.Select("id", "username", "email", "role", "is_active", "created_at", "updated_at").First(&user, user.ID)
	c.JSON(http.StatusCreated, user)
}

func (h *Handler) UpdateUser(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var req struct {
		Email    string `json:"email"`
		Role     string `json:"role"`
		Password string `json:"password"`
		IsActive *bool  `json:"is_active"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if req.Role != "" && !models.IsValidUserRole(req.Role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "角色无效，仅支持 viewer、operator、change_admin、system_admin 或 auditor"})
		return
	}

	var user models.User
	if err := h.db.First(&user, id).Error; err != nil {
		utils.HandleNotFoundError(c, "user")
		return
	}

	originalVersion := user.SessionVersion
	if req.Email != "" {
		user.Email = req.Email
	}
	if req.Role != "" && req.Role != user.Role {
		currentUserID, _ := c.Get("user_id")
		if currentUserID == user.ID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不能修改当前登录用户的角色"})
			return
		}
		user.Role = req.Role
	}
	if req.Password != "" {
		if !utils.ValidPasswordLength(req.Password) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "密码必须为 1–72 UTF-8 字节"})
			return
		}
		hashed := utils.HashPassword(req.Password)
		if hashed == "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "密码加密失败"})
			return
		}
		user.Password = hashed
		user.SessionVersion++
		user.MustChangePassword = true
	}
	if req.IsActive != nil {
		currentUserID, _ := c.Get("user_id")
		if !*req.IsActive && currentUserID == user.ID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不能停用当前登录用户"})
			return
		}
		user.IsActive = *req.IsActive
	}

	result := h.db.Model(&user).Where("id = ? AND session_version = ?", user.ID, originalVersion).
		Updates(map[string]interface{}{
			"email": user.Email, "role": user.Role, "is_active": user.IsActive,
			"password": user.Password, "session_version": user.SessionVersion,
			"must_change_password": user.MustChangePassword,
		})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新用户失败"})
		return
	}

	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"error": "用户已被其他请求更新，请重试"})
		return
	}
	h.db.Select("id", "username", "email", "role", "is_active", "created_at", "updated_at").First(&user, user.ID)
	c.JSON(http.StatusOK, user)
}

func (h *Handler) DeleteUser(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var user models.User
	if err := h.db.First(&user, id).Error; err != nil {
		utils.HandleNotFoundError(c, "user")
		return
	}
	currentUserID, _ := c.Get("user_id")
	if currentUserID == user.ID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能删除当前登录用户"})
		return
	}
	if err := h.db.Delete(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除用户失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "已删除"})
}
