package scheduler

import (
	"context"
	"sync"

	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

// Scheduler manages background tasks: scheduled backups and inspections, health polling, and alert collection.
type Scheduler struct {
	db                *gorm.DB
	basePath          string
	wg                sync.WaitGroup
	mu                sync.Mutex
	cancel            context.CancelFunc
	running           bool
	stopped           chan struct{}
	lastResourcePrune string
	notifier          *services.NotificationService
}

// NewScheduler creates a new Scheduler instance.
func NewScheduler(db *gorm.DB, basePath string) *Scheduler {
	return &Scheduler{
		db:       db,
		basePath: basePath,
		notifier: services.NewNotificationService(db),
	}
}

// Start begins all scheduler goroutines. Repeated calls while running are
// ignored, matching Stop's idempotent lifecycle contract.
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.stopped = make(chan struct{})
	s.running = true
	s.wg.Add(9)
	s.mu.Unlock()
	go s.runScheduledBackups(ctx)
	go s.runScheduledInspections(ctx)
	go s.healthPoll(ctx)
	go s.collectAlerts(ctx)
	go s.collectResourceUsage(ctx)
	go s.syncConfig(ctx)
	go s.runMaintenanceWatcher(ctx)
	go s.runPlatformDBBackup(ctx)
	go s.refreshVersionsLoop(ctx)
}

// Stop gracefully shuts down all scheduler goroutines. Concurrent and repeated
// calls are safe; only the first caller signals cancellation.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	cancel := s.cancel
	stopped := s.stopped
	if cancel != nil {
		s.cancel = nil
	}
	s.mu.Unlock()

	if cancel == nil {
		if stopped != nil {
			<-stopped
		}
		return
	}
	cancel()
	s.wg.Wait()
	s.mu.Lock()
	s.running = false
	if s.stopped != nil {
		close(s.stopped)
		s.stopped = nil
	}
	s.mu.Unlock()
}
