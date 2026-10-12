package scheduler

import (
	"sync"
	"time"
)

// LoopMetric is a point-in-time copy of one scheduler loop's health.
type LoopMetric struct {
	LastSuccessUnix int64
	Failures        uint64
}

// MetricsSnapshot is safe for consumers outside the scheduler package.
type MetricsSnapshot struct {
	Loops                map[string]LoopMetric
	ConfigReloadFailures uint64
	BackupLastSuccess    int64
}

var schedulerMetrics = struct {
	sync.RWMutex
	loops                map[string]LoopMetric
	configReloadFailures uint64
	backupLastSuccess    int64
}{loops: map[string]LoopMetric{}}

func observeLoop(name string, err error) {
	schedulerMetrics.Lock()
	metric := schedulerMetrics.loops[name]
	if err != nil {
		metric.Failures++
	} else {
		metric.LastSuccessUnix = time.Now().Unix()
	}
	schedulerMetrics.loops[name] = metric
	schedulerMetrics.Unlock()
}

func observeConfigReloadFailure() {
	schedulerMetrics.Lock()
	schedulerMetrics.configReloadFailures++
	schedulerMetrics.Unlock()
}

func observeBackupSuccess(at time.Time) {
	schedulerMetrics.Lock()
	if at.Unix() > schedulerMetrics.backupLastSuccess {
		schedulerMetrics.backupLastSuccess = at.Unix()
	}
	schedulerMetrics.Unlock()
}

func SchedulerMetricsSnapshot() MetricsSnapshot {
	schedulerMetrics.RLock()
	defer schedulerMetrics.RUnlock()
	loops := make(map[string]LoopMetric, len(schedulerMetrics.loops))
	for name, metric := range schedulerMetrics.loops {
		loops[name] = metric
	}
	return MetricsSnapshot{
		Loops: loops, ConfigReloadFailures: schedulerMetrics.configReloadFailures,
		BackupLastSuccess: schedulerMetrics.backupLastSuccess,
	}
}
