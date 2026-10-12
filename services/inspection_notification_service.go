package services

import (
	"fmt"
	"sync"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

var inspectionNotificationMu sync.Mutex

type InspectionNotificationEvent struct {
	TransitionID        uint                   `json:"transition_id"`
	ComponentID         uint                   `json:"component_id"`
	Component           string                 `json:"component"`
	Event               string                 `json:"event"`
	ConsecutiveFailures int                    `json:"consecutive_failures"`
	Message             string                 `json:"message"`
	Deliveries          []NotificationDelivery `json:"deliveries"`
}

type InspectionNotificationService struct {
	db       *gorm.DB
	notifier *NotificationService
}

func NewInspectionNotificationService(db *gorm.DB) *InspectionNotificationService {
	return &InspectionNotificationService{db: db, notifier: NewNotificationService(db)}
}
func (s *InspectionNotificationService) Policy() (models.InspectionNotificationPolicy, error) {
	var policy models.InspectionNotificationPolicy
	result := s.db.Order("id ASC").Limit(1).Find(&policy)
	if result.Error == nil && result.RowsAffected == 0 {
		return models.InspectionNotificationPolicy{Enabled: false, ConsecutiveFailures: 2, NotifyRecovery: true, ChannelIDs: []uint{}}, nil
	}
	return policy, result.Error
}

// Process consumes persisted evidence once; caller-supplied check bodies are never trusted.
func (s *InspectionNotificationService) Process(run models.InspectionRun, _ []models.InspectionCheck) ([]InspectionNotificationEvent, error) {
	inspectionNotificationMu.Lock()
	defer inspectionNotificationMu.Unlock()
	if run.ID == 0 {
		return nil, fmt.Errorf("inspection evidence has no run ID")
	}
	if err := s.db.First(&run, run.ID).Error; err != nil {
		return nil, err
	}
	if run.Status != "completed" {
		return []InspectionNotificationEvent{}, nil
	}
	var checks []models.InspectionCheck
	if err := s.db.Where("run_id = ?", run.ID).Order("id ASC").Find(&checks).Error; err != nil {
		return nil, err
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		policy, err := (&InspectionNotificationService{db: tx}).Policy()
		if err != nil {
			return err
		}
		if policy.ConsecutiveFailures < 1 {
			policy.ConsecutiveFailures = 2
		}
		for _, check := range checks {
			if check.Status != "failed" && check.Status != "passed" {
				continue
			}
			var component models.Component
			if err := tx.First(&component, check.ComponentID).Error; err == gorm.ErrRecordNotFound {
				continue
			} else if err != nil {
				return err
			}
			var state models.InspectionNotificationState
			if err := tx.Where("component_id = ?", component.ID).First(&state).Error; err != nil && err != gorm.ErrRecordNotFound {
				return err
			}
			if state.LastInspectionRunID >= run.ID {
				continue
			}
			if state.ID == 0 {
				state.ComponentID = component.ID
				state.State = "healthy"
			}
			previous := state.State
			if check.Status == "failed" && state.ConsecutiveFailures == 0 {
				state.OutageRunID, state.LastNotifiedAt = run.ID, nil
			} else if state.OutageRunID == 0 && state.ConsecutiveFailures > 0 {
				state.OutageRunID = state.LastInspectionRunID
			}
			var failureTransitions int64
			if state.OutageRunID != 0 {
				if err := tx.Model(&models.InspectionTransition{}).Where("component_id = ? AND event = ? AND run_id >= ?", component.ID, "failure", state.OutageRunID).Count(&failureTransitions).Error; err != nil {
					return err
				}
			}
			event := ""
			state.LastInspectionRunID, state.LastStatus, state.LastMessage = run.ID, check.Status, check.Message
			if check.Status == "failed" {
				state.ConsecutiveFailures++
				state.State = "pending_failure"
				if state.ConsecutiveFailures >= policy.ConsecutiveFailures {
					state.State = "alerting"
					if failureTransitions == 0 && state.LastNotifiedAt == nil {
						event = "failure"
					}
				}
			} else {
				state.ConsecutiveFailures = 0
				state.State = "healthy"
				if previous == "alerting" && policy.NotifyRecovery && (failureTransitions > 0 || state.LastNotifiedAt != nil) {
					event = "recovery"
				}
				state.OutageRunID, state.LastNotifiedAt = 0, nil
			}
			if event != "" && policy.Enabled {
				transition := models.InspectionTransition{ComponentID: component.ID, RunID: run.ID, Event: event, ConsecutiveFailures: state.ConsecutiveFailures, Message: fmt.Sprintf("巡检 %s：组件 %s（%s），连续失败 %d 次。%s", event, component.Name, component.Type, state.ConsecutiveFailures, check.Message)}
				if err := tx.Create(&transition).Error; err != nil {
					return err
				}
				seen := map[uint]bool{}
				for _, channelID := range policy.ChannelIDs {
					if seen[channelID] {
						continue
					}
					seen[channelID] = true
					delivery := models.InspectionDelivery{TransitionID: transition.ID, ChannelID: channelID, Status: "pending", NextAttemptAt: time.Now()}
					if err := tx.Create(&delivery).Error; err != nil {
						return err
					}
				}
			}
			if err := tx.Save(&state).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.retryPending(time.Now())
}

func (s *InspectionNotificationService) RetryPending(now time.Time) ([]InspectionNotificationEvent, error) {
	inspectionNotificationMu.Lock()
	defer inspectionNotificationMu.Unlock()
	return s.retryPending(now)
}

func (s *InspectionNotificationService) retryPending(now time.Time) ([]InspectionNotificationEvent, error) {
	events := []InspectionNotificationEvent{}
	policy, err := s.Policy()
	if err != nil {
		return events, err
	}
	if !policy.Enabled {
		return events, nil
	}
	var pending []models.InspectionDelivery
	if err := s.db.Where("status IN ? AND next_attempt_at <= ?", []string{"pending", "failed"}, now).Order("transition_id ASC, channel_id ASC").Find(&pending).Error; err != nil {
		return events, err
	}
	for _, delivery := range pending {
		var transition models.InspectionTransition
		if err := s.db.First(&transition, delivery.TransitionID).Error; err != nil {
			return events, err
		}
		// Preserve failure/recovery order per component and channel, even across outages.
		var earlier int64
		if err := s.db.Model(&models.InspectionDelivery{}).Joins("JOIN inspection_transitions ON inspection_transitions.id = inspection_deliveries.transition_id").Where("inspection_transitions.component_id = ? AND inspection_deliveries.channel_id = ? AND inspection_deliveries.transition_id < ? AND inspection_deliveries.status <> ?", transition.ComponentID, delivery.ChannelID, transition.ID, "sent").Count(&earlier).Error; err != nil {
			return events, err
		}
		if earlier > 0 {
			continue
		}
		claim := s.db.Model(&models.InspectionDelivery{}).Where("id = ? AND status = ? AND attempts = ? AND next_attempt_at <= ?", delivery.ID, delivery.Status, delivery.Attempts, now).Updates(map[string]interface{}{"status": "sending", "attempts": gorm.Expr("attempts + 1")})
		if claim.Error != nil {
			return events, claim.Error
		}
		if claim.RowsAffected == 0 {
			continue
		}
		var component models.Component
		var channel models.NotificationChannel
		status, lastError := "failed", "notification component or channel unavailable/disabled"
		if s.db.First(&component, transition.ComponentID).Error == nil && s.db.First(&channel, delivery.ChannelID).Error == nil && channel.Enabled && (channel.ClusterID == nil || *channel.ClusterID == component.ClusterID) {
			severity, eventName := "critical", "InspectionComponentFailure"
			if transition.Event == "recovery" {
				severity, eventName = "info", "InspectionComponentRecovery"
			}
			status, lastError = s.notifier.deliver(&channel, eventName, transition.Message, severity)
			if err := s.db.Create(&models.AlertNotification{AlertName: eventName, Severity: severity, ChannelID: channel.ID, ChannelType: channel.Type, Message: transition.Message, Status: status, Error: lastError}).Error; err != nil {
				return events, err
			}
		}
		updates := map[string]interface{}{"status": status, "last_error": lastError, "next_attempt_at": now.Add(time.Minute)}
		if status == "sent" {
			updates["acknowledged_at"] = time.Now()
		}
		if err := s.db.Model(&models.InspectionDelivery{}).Where("id = ? AND status = ?", delivery.ID, "sending").Updates(updates).Error; err != nil {
			return events, err
		}
		if status == "sent" {
			if err := s.db.Model(&models.InspectionNotificationState{}).Where("component_id = ? AND last_inspection_run_id = ?", transition.ComponentID, transition.RunID).Update("last_notified_at", time.Now()).Error; err != nil {
				return events, err
			}
		}
		eventIndex := -1
		for index := range events {
			if events[index].TransitionID == transition.ID {
				eventIndex = index
				break
			}
		}
		if eventIndex < 0 {
			events = append(events, InspectionNotificationEvent{TransitionID: transition.ID, ComponentID: component.ID, Component: component.Name, Event: transition.Event, ConsecutiveFailures: transition.ConsecutiveFailures, Message: transition.Message, Deliveries: []NotificationDelivery{}})
			eventIndex = len(events) - 1
		}
		events[eventIndex].Deliveries = append(events[eventIndex].Deliveries, NotificationDelivery{ChannelID: delivery.ChannelID, ChannelName: channel.Name, ChannelType: channel.Type, Status: status, Error: lastError})
	}
	return events, nil
}
