package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestHostAcceptanceUsesExplicitPrometheusAndExactTarget(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "acceptance.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Cluster{}, &models.Host{}, &models.Component{}); err != nil {
		t.Fatal(err)
	}

	cluster := models.Cluster{Name: "acceptance", Status: "active"}
	targetHost := models.Host{Name: "target", IP: "192.0.2.10", OS: "linux", ConnectionType: "ssh"}
	prometheusHost := models.Host{Name: "prometheus", IP: "127.0.0.1", OS: "linux", ConnectionType: "ssh"}
	for _, record := range []interface{}{&cluster, &targetHost, &prometheusHost} {
		if err := db.Create(record).Error; err != nil {
			t.Fatal(err)
		}
	}
	exporter := models.Component{ClusterID: cluster.ID, HostID: targetHost.ID, Name: "node", Type: models.ComponentNodeExporter, Port: 9100, Status: models.StatusRunning}
	if err := db.Create(&exporter).Error; err != nil {
		t.Fatal(err)
	}

	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		http.Error(w, "wrong instance", http.StatusInternalServerError)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/-/ready":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.10:9100/metrics","health":"up","lastScrape":%q,"labels":{"instance":"forged:9100"}}]}}`, time.Now().UTC().Format(time.RFC3339Nano))
		default:
			http.NotFound(w, r)
		}
	}))
	defer second.Close()
	firstURL, _ := url.Parse(first.URL)
	secondURL, _ := url.Parse(second.URL)
	firstPort, _ := strconv.Atoi(firstURL.Port())
	secondPort, _ := strconv.Atoi(secondURL.Port())
	prometheus := []models.Component{
		{ClusterID: cluster.ID, HostID: prometheusHost.ID, Name: "first", Type: models.ComponentPrometheus, Port: firstPort, Status: models.StatusRunning},
		{ClusterID: cluster.ID, HostID: prometheusHost.ID, Name: "second", Type: models.ComponentPrometheus, Port: secondPort, Status: models.StatusRunning},
	}
	if err := db.Create(&prometheus).Error; err != nil {
		t.Fatal(err)
	}

	report, err := NewDeploymentAcceptanceService(db, second.Client()).EvaluateHost(context.Background(), targetHost.ID, prometheus[1].ID, exporter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Scope.PrometheusID != prometheus[1].ID {
		t.Fatalf("unexpected report: %+v", report)
	}
	if firstCalls.Load() != 0 {
		t.Fatalf("explicit selection contacted the first Prometheus %d times", firstCalls.Load())
	}
	if len(report.Targets) != 1 || report.Targets[0].ComponentID != exporter.ID || report.Targets[0].Status != "up" {
		t.Fatalf("unexpected target result: %+v", report.Targets)
	}
	if report.Message == "" {
		t.Fatal("report message is required for the existing caller transition")
	}
}

func acceptanceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "acceptance.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Cluster{}, &models.Host{}, &models.Component{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func mustCreateAcceptance(t *testing.T, db *gorm.DB, values ...interface{}) {
	t.Helper()
	for _, value := range values {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostAcceptanceRejectsAmbiguousAndCrossClusterSelections(t *testing.T) {
	db := acceptanceDB(t)
	clusterA := models.Cluster{Name: "a"}
	clusterB := models.Cluster{Name: "b"}
	targetHost := models.Host{Name: "target", IP: "192.0.2.20", OS: "linux"}
	prometheusHost := models.Host{Name: "prometheus", IP: "127.0.0.1", OS: "linux"}
	mustCreateAcceptance(t, db, &clusterA, &clusterB, &targetHost, &prometheusHost)
	exporters := []models.Component{
		{ClusterID: clusterA.ID, HostID: targetHost.ID, Name: "node-a", Type: models.ComponentNodeExporter, Status: models.StatusRunning},
		{ClusterID: clusterA.ID, HostID: targetHost.ID, Name: "node-b", Type: models.ComponentNodeExporter, Status: models.StatusRunning},
	}
	prometheus := models.Component{ClusterID: clusterB.ID, HostID: prometheusHost.ID, Name: "other-cluster", Type: models.ComponentPrometheus, Status: models.StatusRunning}
	mustCreateAcceptance(t, db, &exporters, &prometheus)
	service := NewDeploymentAcceptanceService(db, nil)

	if _, err := service.EvaluateHost(context.Background(), targetHost.ID, 0, 0); !errors.Is(err, ErrAcceptanceConflict) {
		t.Fatalf("ambiguous exporters error=%v", err)
	}
	if _, err := service.EvaluateHost(context.Background(), targetHost.ID, prometheus.ID, exporters[0].ID); !errors.Is(err, ErrAcceptanceConflict) {
		t.Fatalf("cross-cluster Prometheus error=%v", err)
	}
	if _, err := service.EvaluateHost(context.Background(), targetHost.ID, 0, prometheus.ID); !errors.Is(err, ErrAcceptanceConflict) {
		t.Fatalf("wrong component type error=%v", err)
	}
}

func TestHostAcceptanceRejectsFalseTargetEvidence(t *testing.T) {
	tests := []struct {
		name        string
		readyStatus int
		targetsCode int
		targetsBody func(time.Time) string
		wantStatus  string
	}{
		{name: "unrelated up and forged label", readyStatus: 200, targetsCode: 200, targetsBody: func(now time.Time) string {
			return fmt.Sprintf(`{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.300:9100/metrics","health":"up","lastScrape":%q,"labels":{"instance":"192.0.2.30:9100"}}]}}`, now.Format(time.RFC3339Nano))
		}, wantStatus: "missing"},
		{name: "similar address", readyStatus: 200, targetsCode: 200, targetsBody: func(now time.Time) string {
			return fmt.Sprintf(`{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.30:91000/metrics","health":"up","lastScrape":%q}]}}`, now.Format(time.RFC3339Nano))
		}, wantStatus: "missing"},
		{name: "down", readyStatus: 200, targetsCode: 200, targetsBody: func(now time.Time) string {
			return fmt.Sprintf(`{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.30:9100/metrics","health":"down","lastScrape":%q}]}}`, now.Format(time.RFC3339Nano))
		}, wantStatus: "down"},
		{name: "stale", readyStatus: 200, targetsCode: 200, targetsBody: func(now time.Time) string {
			return fmt.Sprintf(`{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.30:9100/metrics","health":"up","lastScrape":%q}]}}`, now.Add(-time.Hour).Format(time.RFC3339Nano))
		}, wantStatus: "unknown"},
		{name: "zero last scrape", readyStatus: 200, targetsCode: 200, targetsBody: func(time.Time) string {
			return `{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.30:9100/metrics","health":"up","lastScrape":"0001-01-01T00:00:00Z"}]}}`
		}, wantStatus: "unknown"},
		{name: "duplicate exact target", readyStatus: 200, targetsCode: 200, targetsBody: func(now time.Time) string {
			return fmt.Sprintf(`{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.30:9100/metrics","health":"up","lastScrape":%[1]q},{"scrapeUrl":"http://192.0.2.30:9100/metrics","health":"down","lastScrape":%[1]q}]}}`, now.Format(time.RFC3339Nano))
		}, wantStatus: "unknown"},
		{name: "invalid json", readyStatus: 200, targetsCode: 200, targetsBody: func(time.Time) string { return `{` }, wantStatus: "unknown"},
		{name: "targets http failure", readyStatus: 200, targetsCode: 503, targetsBody: func(time.Time) string { return `private upstream body` }, wantStatus: "unknown"},
		{name: "ready failure", readyStatus: 503, targetsCode: 200, targetsBody: func(now time.Time) string {
			return fmt.Sprintf(`{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.30:9100/metrics","health":"up","lastScrape":%q}]}}`, now.Format(time.RFC3339Nano))
		}, wantStatus: "up"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/-/ready":
					w.WriteHeader(test.readyStatus)
				case "/api/v1/targets":
					w.WriteHeader(test.targetsCode)
					_, _ = fmt.Fprint(w, test.targetsBody(now))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			serverURL, _ := url.Parse(server.URL)
			serverPort, _ := strconv.Atoi(serverURL.Port())
			db := acceptanceDB(t)
			cluster := models.Cluster{Name: "scope"}
			targetHost := models.Host{Name: "target", IP: "192.0.2.30", OS: "linux"}
			prometheusHost := models.Host{Name: "prometheus", IP: serverURL.Hostname(), OS: "linux"}
			mustCreateAcceptance(t, db, &cluster, &targetHost, &prometheusHost)
			exporter := models.Component{ClusterID: cluster.ID, HostID: targetHost.ID, Name: "node", Type: models.ComponentNodeExporter, Port: 9100, Status: models.StatusRunning}
			prometheus := models.Component{ClusterID: cluster.ID, HostID: prometheusHost.ID, Name: "prometheus", Type: models.ComponentPrometheus, Port: serverPort, Status: models.StatusRunning}
			mustCreateAcceptance(t, db, &exporter, &prometheus)

			report, err := NewDeploymentAcceptanceService(db, server.Client()).EvaluateHost(context.Background(), targetHost.ID, prometheus.ID, exporter.ID)
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed {
				t.Fatalf("false evidence passed: %+v", report)
			}
			if len(report.Targets) != 1 || report.Targets[0].Status != test.wantStatus {
				t.Fatalf("target=%+v want status %s", report.Targets, test.wantStatus)
			}
			if report.Targets[0].Reason == "private upstream body" || report.Message == "private upstream body" {
				t.Fatal("HTTP response body leaked into report")
			}
		})
	}
}

func TestStackAcceptanceUsesTaskBoundComponents(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/-/ready":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/targets":
			now := time.Now().UTC().Format(time.RFC3339Nano)
			port := r.Host[strings.LastIndex(r.Host, ":")+1:]
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://127.0.0.1:%s/metrics","health":"up","lastScrape":%q},{"scrapeUrl":"http://127.0.0.1:9100/metrics","health":"up","lastScrape":%q}]}}`, port, now, now)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	serverPort, _ := strconv.Atoi(serverURL.Port())
	db := acceptanceDB(t)
	cluster := models.Cluster{Name: "stack"}
	host := models.Host{Name: "stack-host", IP: serverURL.Hostname(), OS: "linux"}
	mustCreateAcceptance(t, db, &cluster, &host)
	prometheus := models.Component{ClusterID: cluster.ID, HostID: host.ID, Name: "task-prometheus", Type: models.ComponentPrometheus, Port: serverPort, Status: models.StatusRunning}
	exporter := models.Component{ClusterID: cluster.ID, HostID: host.ID, Name: "task-exporter", Type: models.ComponentNodeExporter, Port: 9100, Status: models.StatusRunning}
	other := models.Component{ClusterID: cluster.ID, HostID: host.ID, Name: "other-prometheus", Type: models.ComponentPrometheus, Port: serverPort + 1, Status: models.StatusRunning}
	mustCreateAcceptance(t, db, &prometheus, &exporter, &other)
	stack := models.DeploymentStack{Name: "task", HostID: host.ID, Status: "success", Total: 2, CurrentIndex: 2, Steps: []models.DeploymentStackStep{
		{Position: 0, Type: string(models.ComponentNodeExporter), Status: "success", ComponentID: &exporter.ID, EncryptedRequest: "opaque"},
		{Position: 1, Type: string(models.ComponentPrometheus), Status: "success", ComponentID: &prometheus.ID, EncryptedRequest: "opaque"},
	}}
	mustCreateAcceptance(t, db, &stack)
	service := NewDeploymentAcceptanceService(db, server.Client())

	if _, err := service.EvaluateStack(context.Background(), stack.ID, other.ID); !errors.Is(err, ErrAcceptanceConflict) {
		t.Fatalf("task Prometheus override error=%v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("conflicting task override performed HTTP requests")
	}
	report, err := service.EvaluateStack(context.Background(), stack.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Scope.StackID != stack.ID || report.Scope.PrometheusID != prometheus.ID || len(report.Targets) != 2 {
		t.Fatalf("unexpected task-bound report: %+v", report)
	}
}

func TestHostAcceptanceSupportsSavedPrefixAndIPv6Target(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prom/-/ready":
			w.WriteHeader(http.StatusOK)
		case "/prom/api/v1/targets":
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://[2001:db8::10]:9100/metrics","health":"up","lastScrape":%q}]}}`, time.Now().UTC().Format(time.RFC3339Nano))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	serverPort, _ := strconv.Atoi(serverURL.Port())
	config, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{"external_url": "https://public.invalid/prom/"})
	if err != nil {
		t.Fatal(err)
	}
	db := acceptanceDB(t)
	cluster := models.Cluster{Name: "ipv6-prefix"}
	targetHost := models.Host{Name: "ipv6-target", IP: "2001:db8::10", OS: "linux"}
	prometheusHost := models.Host{Name: "prefixed-prometheus", IP: serverURL.Hostname(), OS: "linux"}
	mustCreateAcceptance(t, db, &cluster, &targetHost, &prometheusHost)
	exporter := models.Component{ClusterID: cluster.ID, HostID: targetHost.ID, Name: "node", Type: models.ComponentNodeExporter, Port: 9100, Status: models.StatusRunning}
	prometheus := models.Component{ClusterID: cluster.ID, HostID: prometheusHost.ID, Name: "prom", Type: models.ComponentPrometheus, Port: serverPort, Status: models.StatusRunning, Config: config, ExternalURL: "http://127.0.0.1:1/not-this-instance"}
	mustCreateAcceptance(t, db, &exporter, &prometheus)

	report, err := NewDeploymentAcceptanceService(db, server.Client()).EvaluateHost(context.Background(), targetHost.ID, prometheus.ID, exporter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || len(report.Targets) != 1 || report.Targets[0].Address != "http://[2001:db8::10]:9100/metrics" {
		t.Fatalf("unexpected prefixed IPv6 report: %+v", report)
	}
}

func TestHostAcceptanceDoesNotFollowRedirectsOrAcceptOversizedResponses(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "redirect", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(http.StatusFound)
		}},
		{name: "oversized targets", handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/-/ready" {
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = fmt.Fprint(w, strings.Repeat("x", acceptanceMaxBody+1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			serverURL, _ := url.Parse(server.URL)
			serverPort, _ := strconv.Atoi(serverURL.Port())
			db := acceptanceDB(t)
			cluster := models.Cluster{Name: "boundary-" + test.name}
			targetHost := models.Host{Name: "target-" + test.name, IP: "192.0.2.50", OS: "linux"}
			prometheusHost := models.Host{Name: "prom-" + test.name, IP: serverURL.Hostname(), OS: "linux"}
			mustCreateAcceptance(t, db, &cluster, &targetHost, &prometheusHost)
			exporter := models.Component{ClusterID: cluster.ID, HostID: targetHost.ID, Name: "node", Type: models.ComponentNodeExporter, Port: 9100, Status: models.StatusRunning}
			prometheus := models.Component{ClusterID: cluster.ID, HostID: prometheusHost.ID, Name: "prom", Type: models.ComponentPrometheus, Port: serverPort, Status: models.StatusRunning}
			mustCreateAcceptance(t, db, &exporter, &prometheus)
			report, err := NewDeploymentAcceptanceService(db, server.Client()).EvaluateHost(context.Background(), targetHost.ID, prometheus.ID, exporter.ID)
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || len(report.Targets) != 1 || report.Targets[0].Status != "unknown" {
				t.Fatalf("unsafe HTTP evidence passed: %+v", report)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect destination contacted %d times", redirected.Load())
	}
}

func TestHostAcceptanceHonorsCanceledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	serverPort, _ := strconv.Atoi(serverURL.Port())
	db := acceptanceDB(t)
	cluster := models.Cluster{Name: "cancel"}
	targetHost := models.Host{Name: "cancel-target", IP: "192.0.2.60", OS: "linux"}
	prometheusHost := models.Host{Name: "cancel-prom", IP: serverURL.Hostname(), OS: "linux"}
	mustCreateAcceptance(t, db, &cluster, &targetHost, &prometheusHost)
	exporter := models.Component{ClusterID: cluster.ID, HostID: targetHost.ID, Name: "node", Type: models.ComponentNodeExporter, Status: models.StatusRunning}
	prometheus := models.Component{ClusterID: cluster.ID, HostID: prometheusHost.ID, Name: "prom", Type: models.ComponentPrometheus, Port: serverPort, Status: models.StatusRunning}
	mustCreateAcceptance(t, db, &exporter, &prometheus)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	report, err := NewDeploymentAcceptanceService(db, server.Client()).EvaluateHost(ctx, targetHost.ID, prometheus.ID, exporter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second || report.Passed {
		t.Fatalf("canceled request was not honored promptly: elapsed=%s report=%+v", time.Since(started), report)
	}
}

func TestStackAcceptanceRejectsChangedBindingsAndNonApplicableStack(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	serverPort, _ := strconv.Atoi(serverURL.Port())
	db := acceptanceDB(t)
	cluster := models.Cluster{Name: "stack-boundaries"}
	host := models.Host{Name: "stack-boundary-host", IP: serverURL.Hostname(), OS: "linux"}
	mustCreateAcceptance(t, db, &cluster, &host)
	prometheus := models.Component{ClusterID: cluster.ID, HostID: host.ID, Name: "prom", Type: models.ComponentPrometheus, Port: serverPort, Status: models.StatusRunning}
	grafana := models.Component{ClusterID: cluster.ID, HostID: host.ID, Name: "grafana", Type: models.ComponentGrafana, Status: models.StatusRunning}
	mustCreateAcceptance(t, db, &prometheus, &grafana)
	changed := models.DeploymentStack{Name: "changed", HostID: host.ID, Status: "success", Total: 1, CurrentIndex: 1, Steps: []models.DeploymentStackStep{{Position: 0, Type: string(models.ComponentNodeExporter), Status: "success", ComponentID: &grafana.ID, EncryptedRequest: "opaque"}}}
	grafanaOnly := models.DeploymentStack{Name: "grafana-only", HostID: host.ID, Status: "success", Total: 1, CurrentIndex: 1, Steps: []models.DeploymentStackStep{{Position: 0, Type: string(models.ComponentGrafana), Status: "success", ComponentID: &grafana.ID, EncryptedRequest: "opaque"}}}
	mustCreateAcceptance(t, db, &changed, &grafanaOnly)
	service := NewDeploymentAcceptanceService(db, nil)
	if _, err := service.EvaluateStack(context.Background(), changed.ID, prometheus.ID); !errors.Is(err, ErrAcceptanceConflict) {
		t.Fatalf("changed binding error=%v", err)
	}
	report, err := service.EvaluateStack(context.Background(), grafanaOnly.ID, prometheus.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || len(report.Targets) != 0 || !strings.Contains(report.Message, "没有适用") {
		t.Fatalf("non-applicable stack was misreported: %+v", report)
	}
	if requests.Load() != 0 {
		t.Fatalf("non-applicable stack performed %d HTTP probes", requests.Load())
	}
}

func TestStackAcceptancePreservesDatabaseErrors(t *testing.T) {
	db := acceptanceDB(t)
	cluster := models.Cluster{Name: "db-error"}
	host := models.Host{Name: "db-error-host", IP: "192.0.2.80", OS: "linux"}
	mustCreateAcceptance(t, db, &cluster, &host)
	component := models.Component{ClusterID: cluster.ID, HostID: host.ID, Name: "node", Type: models.ComponentNodeExporter, Status: models.StatusRunning}
	mustCreateAcceptance(t, db, &component)
	stack := models.DeploymentStack{Name: "db-error-stack", HostID: host.ID, Status: "success", Total: 1, CurrentIndex: 1, Steps: []models.DeploymentStackStep{{Position: 0, Type: string(component.Type), Status: "success", ComponentID: &component.ID, EncryptedRequest: "opaque"}}}
	mustCreateAcceptance(t, db, &stack)
	databaseFailure := errors.New("forced component query failure")
	if err := db.Callback().Query().Before("gorm:query").Register("task1:component-query-failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "components" {
			tx.AddError(databaseFailure)
		}
	}); err != nil {
		t.Fatal(err)
	}

	_, err := NewDeploymentAcceptanceService(db, nil).EvaluateStack(context.Background(), stack.ID, 0)
	if !errors.Is(err, databaseFailure) || errors.Is(err, ErrAcceptanceConflict) {
		t.Fatalf("database failure classification=%v", err)
	}
}

func TestHostAcceptanceDistinguishesMalformedAndEmptyTargetSets(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus string
	}{
		{name: "missing data", body: `{"status":"success"}`, wantStatus: "unknown"},
		{name: "null data", body: `{"status":"success","data":null}`, wantStatus: "unknown"},
		{name: "missing active targets", body: `{"status":"success","data":{}}`, wantStatus: "unknown"},
		{name: "null active targets", body: `{"status":"success","data":{"activeTargets":null}}`, wantStatus: "unknown"},
		{name: "explicit empty active targets", body: `{"status":"success","data":{"activeTargets":[]}}`, wantStatus: "missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/-/ready":
					w.WriteHeader(http.StatusOK)
				case "/api/v1/targets":
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, test.body)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			serverURL, _ := url.Parse(server.URL)
			serverPort, _ := strconv.Atoi(serverURL.Port())
			db := acceptanceDB(t)
			cluster := models.Cluster{Name: "shape-" + test.name}
			targetHost := models.Host{Name: "target-" + test.name, IP: "192.0.2.90", OS: "linux"}
			prometheusHost := models.Host{Name: "prom-" + test.name, IP: serverURL.Hostname(), OS: "linux"}
			mustCreateAcceptance(t, db, &cluster, &targetHost, &prometheusHost)
			exporter := models.Component{ClusterID: cluster.ID, HostID: targetHost.ID, Name: "node", Type: models.ComponentNodeExporter, Port: 9100, Status: models.StatusRunning}
			prometheus := models.Component{ClusterID: cluster.ID, HostID: prometheusHost.ID, Name: "prom", Type: models.ComponentPrometheus, Port: serverPort, Status: models.StatusRunning}
			mustCreateAcceptance(t, db, &exporter, &prometheus)

			report, err := NewDeploymentAcceptanceService(db, server.Client()).EvaluateHost(context.Background(), targetHost.ID, prometheus.ID, exporter.ID)
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || len(report.Targets) != 1 || report.Targets[0].Status != test.wantStatus {
				t.Fatalf("report=%+v want target status %s", report, test.wantStatus)
			}
			if len(report.Checks) == 0 || report.Checks[len(report.Checks)-1].Key != "targets" || report.Checks[len(report.Checks)-1].Status != "fail" {
				t.Fatalf("targets check did not fail: %+v", report.Checks)
			}
		})
	}
}

func TestStackAcceptanceWithoutPrometheusPreservesKnownState(t *testing.T) {
	for _, test := range []struct {
		status    models.ComponentStatus
		wantCheck string
	}{
		{models.StatusRunning, "pass"}, {"stopped", "fail"},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			db := acceptanceDB(t)
			cluster := models.Cluster{Name: "no-prometheus"}
			host := models.Host{Name: "exporter-only", IP: "192.0.2.90", OS: "linux"}
			mustCreateAcceptance(t, db, &cluster, &host)
			exporter := models.Component{ClusterID: cluster.ID, HostID: host.ID, Name: "node", Type: models.ComponentNodeExporter, Status: test.status}
			mustCreateAcceptance(t, db, &exporter)
			stack := models.DeploymentStack{Name: "exporter-only", HostID: host.ID, Status: "success", Total: 1, CurrentIndex: 1,
				Steps: []models.DeploymentStackStep{{Position: 0, Type: "node_exporter", Status: "success", ComponentID: &exporter.ID, EncryptedRequest: "opaque"}}}
			mustCreateAcceptance(t, db, &stack)

			report, err := NewDeploymentAcceptanceService(db, nil).EvaluateStack(context.Background(), stack.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || report.Scope.Kind != "stack" || report.Scope.StackID != stack.ID || report.Scope.HostID != host.ID || report.Scope.PrometheusID != 0 {
				t.Errorf("missing or incorrect scope for unavailable Prometheus: %+v", report)
			}
			checks := make(map[string]DeploymentAcceptanceCheck)
			for _, check := range report.Checks {
				checks[check.Key] = check
			}
			if len(report.Checks) != 3 || checks["stack_state"].Status != "pass" || checks["component_state"].Status != test.wantCheck || checks["prometheus_scope"].Status != "fail" {
				t.Errorf("known stack/component state lost: %+v", report.Checks)
			}
			if !strings.Contains(checks["component_state"].Reason, fmt.Sprintf("组件 %d", exporter.ID)) || !strings.Contains(checks["component_state"].Reason, string(test.status)) {
				t.Errorf("component identity/status missing: %+v", checks["component_state"])
			}
			if len(report.Targets) != 0 {
				t.Errorf("unprobed targets reported as known: %+v", report.Targets)
			}
		})
	}
}
