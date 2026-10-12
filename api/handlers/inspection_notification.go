package handlers

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
)

type inspectionNotificationPolicyInput struct {
	Enabled             bool   `json:"enabled"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	NotifyRecovery      bool   `json:"notify_recovery"`
	ChannelIDs          []uint `json:"channel_ids"`
}

func (h *Handler) GetInspectionNotificationPolicy(c *gin.Context) {
	policy, err := services.NewInspectionNotificationService(h.db).Policy()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取巡检通知策略失败"})
		return
	}
	c.JSON(http.StatusOK, policy)
}

func (h *Handler) UpdateInspectionNotificationPolicy(c *gin.Context) {
	var input inspectionNotificationPolicyInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "巡检通知策略输入无效: " + err.Error()})
		return
	}
	if input.ConsecutiveFailures < 1 || input.ConsecutiveFailures > 10 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "连续异常阈值必须在 1 到 10 之间"})
		return
	}
	seen := make(map[uint]bool, len(input.ChannelIDs))
	channelIDs := make([]uint, 0, len(input.ChannelIDs))
	for _, id := range input.ChannelIDs {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		channelIDs = append(channelIDs, id)
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })
	if input.Enabled && len(channelIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "启用巡检通知时至少选择一个通知渠道"})
		return
	}
	if len(channelIDs) > 0 {
		var count int64
		if err := h.db.Model(&models.NotificationChannel{}).Where("id IN ? AND enabled = ?", channelIDs, true).Count(&count).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "校验通知渠道失败"})
			return
		}
		if count != int64(len(channelIDs)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "所选通知渠道不存在或未启用"})
			return
		}
		var channels []models.NotificationChannel
		if err := h.db.Where("id IN ?", channelIDs).Find(&channels).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取通知渠道失败"})
			return
		}
		for _, channel := range channels {
			if err := config.ValidateInspectionChannel(channel); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": channel.Name + ": " + err.Error()})
				return
			}
		}
	}
	policy, err := services.NewInspectionNotificationService(h.db).Policy()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取巡检通知策略失败"})
		return
	}
	policy.Enabled = input.Enabled
	policy.ConsecutiveFailures = input.ConsecutiveFailures
	policy.NotifyRecovery = input.NotifyRecovery
	policy.ChannelIDs = channelIDs
	policy.UpdatedBy = inspectionActor(c)
	if policy.ID == 0 {
		err = h.db.Create(&policy).Error
	} else {
		err = h.db.Save(&policy).Error
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存巡检通知策略失败"})
		return
	}
	c.JSON(http.StatusOK, policy)
}

type inspectionNotificationStateView struct {
	models.InspectionNotificationState
	Component     string `json:"component"`
	ComponentType string `json:"component_type"`
	Host          string `json:"host"`
}

func (h *Handler) ListInspectionNotificationStates(c *gin.Context) {
	var states []models.InspectionNotificationState
	if err := h.db.Order("updated_at DESC").Find(&states).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取巡检通知状态失败"})
		return
	}
	componentIDs := make([]uint, 0, len(states))
	for _, state := range states {
		componentIDs = append(componentIDs, state.ComponentID)
	}
	components := make(map[uint]models.Component, len(componentIDs))
	if len(componentIDs) > 0 {
		var records []models.Component
		if err := h.db.Preload("Host").Where("id IN ?", componentIDs).Find(&records).Error; err == nil {
			for _, component := range records {
				components[component.ID] = component
			}
		}
	}
	views := make([]inspectionNotificationStateView, 0, len(states))
	for _, state := range states {
		view := inspectionNotificationStateView{InspectionNotificationState: state}
		if component, exists := components[state.ComponentID]; exists {
			view.Component = component.Name
			view.ComponentType = string(component.Type)
			view.Host = strings.TrimSpace(component.Host.Name)
		}
		views = append(views, view)
	}
	c.JSON(http.StatusOK, gin.H{"states": views})
}
