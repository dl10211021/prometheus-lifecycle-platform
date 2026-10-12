package router

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/api/handlers"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/scheduler"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Removing the upload-only timeout or moving it after SingleNodeScope must
// reject this real chunked upload before the complete archive reaches storage.
func TestPackageUploadSlowBodyAndFollowingRequestDeadline(t *testing.T) {
	setup, db, tokens := uploadRouterFixture(t)
	server := httptest.NewUnstartedServer(setup.Engine)
	server.Config.ReadTimeout = 100 * time.Millisecond
	server.Config.WriteTimeout = 2 * time.Second
	server.Start()
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	payload := []byte("package bytes sent across the normal API read deadline")
	body, contentType := uploadMultipart(t, payload)
	request := slowUploadRequest(t, server.URL+"/api/v1/packages/upload", body, contentType, tokens["system_admin"])
	var firstConn net.Conn
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { firstConn = info.Conn },
	}))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("authorized slow upload status=%d body=%s; want complete persisted package", response.StatusCode, raw)
	}
	var saved models.Package
	if err := db.Where("name = ?", "slow-upload-fixture").First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(saved.StoragePath)
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("stored upload differs: %v", err)
	}
	sum := sha256.Sum256(payload)
	if saved.Size != int64(len(payload)) || saved.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("incomplete package catalog: size=%d checksum=%s", saved.Size, saved.Checksum)
	}

	// Use a genuine ordinary mutation on the SAME connection. Its JSON body must
	// still time out instead of inheriting the authorized upload's longer window.
	request = slowUploadRequest(t, server.URL+"/api/v1/packages", []byte("{\"name\":\"ordinary-must-time-out\",\"type\":\"grafana\"}"), "application/json", tokens["system_admin"])
	reused := false
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused && info.Conn == firstConn },
	}))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("ordinary request did not reuse upload connection")
	}
	if response.StatusCode != http.StatusBadRequest || !bytes.Contains(raw, []byte("读取请求内容失败")) {
		t.Fatalf("ordinary request inherited upload deadline: status=%d body=%s", response.StatusCode, raw)
	}
	var count int64
	if err := db.Model(&models.Package{}).Where("name = ?", "ordinary-must-time-out").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("ordinary request persisted after its read deadline")
	}
}

func TestPackageUploadAuthorizationBeforeBodyRead(t *testing.T) {
	setup, db, tokens := uploadRouterFixture(t)
	for _, role := range []string{"unauthenticated", "invalid_token", "viewer", "operator", "change_admin", "auditor"} {
		t.Run(role, func(t *testing.T) {
			body, contentType := uploadMultipart(t, []byte("must not be read"))
			reader := &uploadCountingReader{Reader: bytes.NewReader(body)}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/packages/upload", reader)
			request.Header.Set("Content-Type", contentType)
			if role == "invalid_token" {
				request.Header.Set("Authorization", "Bearer invalid-token-value")
			} else if token := tokens[role]; token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			setup.Engine.ServeHTTP(response, request)
			want := http.StatusForbidden
			if role == "unauthenticated" || role == "invalid_token" {
				want = http.StatusUnauthorized
			}
			if response.Code != want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if reader.bytesRead.Load() != 0 {
				t.Fatalf("unauthorized upload body consumed %d bytes before rejection", reader.bytesRead.Load())
			}
			if want == http.StatusForbidden {
				var audit models.OperationLog
				if err := db.Where("role = ? AND action = ?", role, "upload_package").Last(&audit).Error; err != nil {
					t.Fatal(err)
				}
				if audit.StatusCode != http.StatusForbidden || audit.Result != "failure" {
					t.Fatalf("denied upload missing audit outcome: %+v", audit)
				}
			}
		})
	}
}

func TestPackageUploadRejectsDeclaredOversizeBeforeBodyRead(t *testing.T) {
	setup, db, tokens := uploadRouterFixture(t)
	body, contentType := uploadMultipart(t, []byte("not an actual 5GiB allocation"))
	reader := &uploadCountingReader{Reader: bytes.NewReader(body)}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/packages/upload", reader)
	request.ContentLength = int64(handlers.MaxUploadSize) + (1 << 20) + 1
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer "+tokens["system_admin"])
	response := httptest.NewRecorder()
	setup.Engine.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || reader.bytesRead.Load() != 0 {
		t.Fatalf("oversize request status=%d read=%d body=%s", response.Code, reader.bytesRead.Load(), response.Body.String())
	}
	var count int64
	if err := db.Model(&models.Package{}).Where("name = ?", "slow-upload-fixture").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("oversize upload persisted")
	}
}

type uploadCountingReader struct {
	io.Reader
	bytesRead atomic.Int64
}

func (r *uploadCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytesRead.Add(int64(n))
	return n, err
}

func slowUploadRequest(t *testing.T, url string, body []byte, contentType, token string) *http.Request {
	t.Helper()
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer writer.Close()
		if _, err := writer.Write(body[:1]); err != nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
		_, _ = writer.Write(body[1:])
	}()
	t.Cleanup(func() { reader.Close(); writer.Close(); <-done })
	request, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func uploadMultipart(t *testing.T, payload []byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, field := range [][2]string{{"name", "slow-upload-fixture"}, {"type", "grafana"}, {"version", "13.1.7+promlifecycle.1"}} {
		if err := form.WriteField(field[0], field[1]); err != nil {
			t.Fatal(err)
		}
	}
	part, err := form.CreateFormFile("file", "grafana.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), form.FormDataContentType()
}

func uploadRouterFixture(t *testing.T) (*SetupResult, *gorm.DB, map[string]string) {
	t.Helper()
	base := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(base, "router.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(

		&models.Host{},
		&models.Cluster{},
		&models.Component{},
		&models.ComponentConfig{},
		&models.Operation{},
		&models.Backup{},
		&models.User{},
		&models.ComponentVersion{},
		&models.OperationLog{},
		&models.AlertRule{},
		&models.AlertHistory{},
		&models.NotificationChannel{},
		&models.AlertNotification{},
		&models.ScheduledBackup{},
		&models.ResourceUsage{},
		&models.MaintenanceWindow{},
		&models.MaintenanceSilence{},
		&models.TopologyEdge{},
		&models.ConfigTemplate{},
		&models.Package{},
		&models.ScrapeConfigModel{},
		&models.RecordingRule{},
		&models.RemoteWriteModel{},
		&models.AlertmanagerRoute{},
		&models.AlertmanagerSettings{},
		&models.AlertSilence{},
		&models.Dashboard{},
		&models.ComponentDefinition{},
		&models.Incident{},
		&models.IncidentEvent{},
		&models.BackupManifest{},
		&models.PrometheusRecoveryPoint{},
		&models.PrometheusRestoreTransaction{},
		&models.ScheduledBackupStatus{},
		&models.RecoveryExercise{},
		&models.ComponentAdoption{},
		&models.OnboardingCheckRun{},
		&models.ChangePlan{},
		&models.InspectionRun{},
		&models.InspectionCheck{},
		&models.ScheduledInspection{},
		&models.ScheduledInspectionStatus{},
		&models.InspectionNotificationPolicy{},
		&models.InspectionNotificationState{},
		&models.AIConfig{},
		&models.AIInteraction{},
		&models.DeploymentPreflight{},
		&models.ComponentOperationClaim{},
		&models.HostOperationClaim{},
		&models.InspectionClaim{},
		&models.InspectionTransition{},
		&models.InspectionDelivery{},
		&models.InspectionDeliveryResolution{},
		&models.ChangeBaseline{},
		&models.DeploymentBatch{},
		&models.DeploymentStack{},
		&models.DeploymentStackStep{},
		&services.ComponentRestoreCheckpoint{},
		&services.ComponentBackupPause{},
		&scheduler.ScheduledBackupRun{},
	); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := services.EnsureSingleNode(db); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "packages"), 0755); err != nil {
		t.Fatal(err)
	}
	// Seed integrity-checked built-ins so the real Setup performs no downloads.
	for kind, version := range deployer.BuiltinVersions {
		var archive bytes.Buffer
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: kind + "/" + kind, Mode: 0755, Size: 7}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("fixture")); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(base, "packages", "fixture-"+kind+".tar.gz")
		if err := os.WriteFile(path, archive.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(archive.Bytes())
		pkg := models.Package{Name: "builtin-fixture-" + kind, Type: models.ComponentType(kind), Version: version, Format: "binary", StoragePath: path, Size: int64(archive.Len()), Checksum: hex.EncodeToString(sum[:])}
		if err := db.Create(&pkg).Error; err != nil {
			t.Fatal(err)
		}
	}
	setup := Setup(db, deployer.NewFactory(base), config.NewManager(base), base)
	t.Cleanup(setup.Stop)
	tokens := make(map[string]string)
	passwordHash := utils.HashPassword("upload-fixture-password")
	for _, role := range []string{"system_admin", "viewer", "operator", "change_admin", "auditor"} {
		user := models.User{Username: "upload-" + role, Password: passwordHash, Role: role, IsActive: true}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("{\"username\":\"upload-"+role+"\",\"password\":\"upload-fixture-password\"}"))
		request.RemoteAddr = fmt.Sprintf("192.0.2.%d:%d", len(tokens)+1, 10000+len(t.Name()))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		setup.Engine.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("login status=%d body=%s", response.Code, response.Body.String())
		}
		var login struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil || login.Token == "" {
			t.Fatalf("login token: %v", err)
		}
		tokens[role] = login.Token
	}
	return setup, db, tokens
}

// A ResponseRecorder deliberately lacks socket deadline support. Fail closed
// instead of accepting an upload with an ineffective or unlimited read window.
func TestPackageUploadDeadlineFailuresDoNotReadOrStore(t *testing.T) {
	setup, db, tokens := uploadRouterFixture(t)
	for _, failure := range []string{"unsupported", "read", "write"} {
		t.Run(failure, func(t *testing.T) {
			body, contentType := uploadMultipart(t, []byte("deadline failure must not persist"))
			reader := &uploadCountingReader{Reader: bytes.NewReader(body)}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/packages/upload", reader)
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Authorization", "Bearer "+tokens["system_admin"])
			response := httptest.NewRecorder()
			var writer http.ResponseWriter = response
			if failure != "unsupported" {
				deadline := &uploadDeadlineWriter{ResponseRecorder: response}
				if failure == "read" {
					deadline.readErr = errors.New("read deadline unavailable")
				} else {
					deadline.writeErr = errors.New("write deadline unavailable")
				}
				writer = deadline
			}
			setup.Engine.ServeHTTP(writer, request)
			if response.Code != http.StatusInternalServerError || reader.bytesRead.Load() != 0 {
				t.Fatalf("deadline failure %s status=%d bytes=%d body=%s", failure, response.Code, reader.bytesRead.Load(), response.Body.String())
			}
			var count int64
			if err := db.Model(&models.Package{}).Where("name = ?", "slow-upload-fixture").Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("deadline failure stored package")
			}
		})
	}
}

func TestPackageUploadChunkedBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	var entered atomic.Bool
	router.POST("/upload", packageUploadLimits(1024), func(c *gin.Context) { entered.Store(true); c.Status(http.StatusCreated) })
	server := httptest.NewServer(router)
	defer server.Close()
	body, contentType := uploadMultipart(t, bytes.Repeat([]byte("x"), 4096))
	// An io.Reader wrapper prevents net/http deriving Content-Length, exercising
	// the stream bound rather than the declared-length fast rejection.
	request, err := http.NewRequest(http.MethodPost, server.URL+"/upload", io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", contentType)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge || entered.Load() {
		t.Fatalf("chunked oversize status=%d downstream=%v", response.StatusCode, entered.Load())
	}
}

func TestPackageUploadMultipartTemporaryFilesAreRemoved(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	// Cross ParseMultipartForm's 32MiB memory threshold without allocating the
	// whole request. Cover both normal and rejected downstream responses, plus a
	// truncated multipart stream whose partially-spooled file must be removed.
	for _, outcome := range []string{"created", "handler_rejected", "truncated"} {
		t.Run(outcome, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			entered := false
			router.POST("/upload", packageUploadLimits(40<<20), func(c *gin.Context) {
				entered = true
				file, header, err := c.Request.FormFile("file")
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if header.Size != 33<<20 {
					t.Fatalf("spooled file size=%d", header.Size)
				}
				if _, ok := file.(*os.File); !ok {
					t.Fatal("fixture did not exercise a disk-spooled multipart file")
				}
				n, err := io.Copy(io.Discard, file)
				if err != nil || n != 33<<20 {
					t.Fatalf("temporary upload unreadable: bytes=%d err=%v", n, err)
				}
				status := http.StatusCreated
				if outcome == "handler_rejected" {
					status = http.StatusConflict
				}
				c.Status(status)
			})
			reader, writer := io.Pipe()
			form := multipart.NewWriter(writer)
			request := httptest.NewRequest(http.MethodPost, "/upload", reader)
			request.Header.Set("Content-Type", form.FormDataContentType())
			done := make(chan error, 1)
			go func() {
				part, err := form.CreateFormFile("file", "grafana.tar.gz")
				if err == nil {
					_, err = io.CopyN(part, uploadZeroReader{}, 33<<20)
				}
				if err == nil && outcome != "truncated" {
					err = form.Close()
				}
				_ = writer.CloseWithError(err)
				done <- err
			}()
			response := &uploadDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			started := time.Now()
			router.ServeHTTP(response, request)
			_ = reader.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			want := http.StatusCreated
			if outcome == "handler_rejected" {
				want = http.StatusConflict
			}
			if outcome == "truncated" {
				want = http.StatusBadRequest
			}
			if response.Code != want || entered != (outcome != "truncated") {
				t.Fatalf("multipart outcome=%s status=%d entered=%v body=%s", outcome, response.Code, entered, response.Body.String())
			}
			entries, err := os.ReadDir(temp)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("multipart left temporary files: %v", entries)
			}
			// Deadline calls are the narrow transport boundary seam; neither may clear
			// a server deadline, and writes retain a finite post-read completion budget.
			if response.readDeadline.Before(started.Add(299*time.Second)) || response.readDeadline.After(started.Add(301*time.Second)) {
				t.Fatalf("upload read window is not bounded near five minutes: %s", response.readDeadline)
			}
			if response.writeDeadline.Before(response.readDeadline.Add(29*time.Second)) || response.writeDeadline.After(response.readDeadline.Add(31*time.Second)) {
				t.Fatalf("upload response deadline lacks finite completion budget: %s", response.writeDeadline)
			}
		})
	}
}

type uploadDeadlineWriter struct {
	*httptest.ResponseRecorder
	readErr, writeErr           error
	readDeadline, writeDeadline time.Time
}

func (w *uploadDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline
	return w.readErr
}
func (w *uploadDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return w.writeErr
}

type uploadZeroReader struct{}

func (uploadZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
