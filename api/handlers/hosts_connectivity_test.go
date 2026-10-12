package handlers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	gossh "golang.org/x/crypto/ssh"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func connectivityHandler(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "hosts.db")+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	h := &Handler{db: db}
	router := gin.New()
	router.POST("/hosts", h.CreateHost)
	router.POST("/hosts/batch", h.BatchCreateHosts)
	router.POST("/hosts/:id/test", h.TestHostConnection)
	return h, router
}

func connectivityRequest(t *testing.T, router http.Handler, path string, payload interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// A real SSH peer exercises authentication, exec requests, remote output and
// exit status. Failure output deliberately contains a credential-like secret.
func connectivitySSH(t *testing.T, success bool) int {
	t.Helper()
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "insecure")
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &gossh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		server, channels, requests, err := gossh.NewServerConn(raw, config)
		if err != nil {
			return
		}
		defer server.Close()
		go gossh.DiscardRequests(requests)
		for incoming := range channels {
			channel, requests, err := incoming.Accept()
			if err != nil {
				return
			}
			for request := range requests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				_ = request.Reply(true, nil)
				code := uint32(1)
				if success {
					_, _ = io.WriteString(channel, "Linux fixture 6.6.0 x86_64\n")
					code = 0
				} else {
					_, _ = io.WriteString(channel.Stderr(), "private-password-marker")
				}
				_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{code}))
				_ = channel.Close()
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-finished:
		case <-time.After(6 * time.Second):
			t.Error("SSH fixture did not stop")
		}
	})
	return listener.Addr().(*net.TCPAddr).Port
}

func TestCreateHostDuplicatePreservesExistingCredentials(t *testing.T) {
	h, router := connectivityHandler(t)
	existing := models.Host{Name: "original", IP: "127.0.0.1", ConnectionType: "ssh", Username: "original-user", Password: "original-password", Status: "offline", LastError: "previous failure"}
	if err := h.db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	response := connectivityRequest(t, router, "/hosts", hostCreateRequest{Name: "replacement", IP: existing.IP, Username: "changed", Password: "changed", VerifyConnection: true})
	if response.Code != http.StatusOK {
		t.Fatalf("duplicate response %d: %s", response.Code, response.Body)
	}
	var result struct {
		Status string
		ID     uint
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "exists" || result.ID != existing.ID {
		t.Fatalf("duplicate result: %+v", result)
	}
	var stored models.Host
	if err := h.db.First(&stored, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Name != "original" || stored.Username != "original-user" || stored.Password != "original-password" || stored.Status != "offline" || stored.LastError != "previous failure" || stored.LastCheckedAt != nil {
		t.Fatalf("duplicate changed existing row: %+v", stored)
	}
}

func TestBatchCreateHostsVerificationAndOrderedDuplicates(t *testing.T) {
	h, router := connectivityHandler(t)
	port := connectivitySSH(t, false)
	requests := []hostCreateRequest{
		{Name: "local", IP: "127.0.0.2", ConnectionType: "local", VerifyConnection: true},
		{Name: "fails", IP: "127.0.0.1", Port: port, Username: "root", Password: "private-password-marker", VerifyConnection: true},
		{Name: "unchecked", IP: "127.0.0.3", Username: "root"},
		{Name: "replacement", IP: "127.0.0.2", Username: "changed", Password: "changed", VerifyConnection: true},
	}
	response := connectivityRequest(t, router, "/hosts/batch", requests)
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("batch response %d: %s", response.Code, response.Body)
	}
	var result struct {
		Results []struct {
			Index         int
			ID            uint
			Status, Error string
		}
		Failed int
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 || len(result.Results) != 4 {
		t.Fatalf("batch result: %+v", result)
	}
	for index, status := range []string{"created", "failed", "created", "exists"} {
		item := result.Results[index]
		if item.Index != index || item.Status != status {
			t.Fatalf("result %d: %+v", index, item)
		}
	}
	if result.Results[1].Error == "" || strings.Contains(response.Body.String(), "private-password-marker") {
		t.Fatalf("unsafe/missing error: %s", response.Body)
	}
	if result.Results[0].ID != result.Results[3].ID {
		t.Fatal("duplicate returned a different record")
	}
	var hosts []models.Host
	if err := h.db.Order("ip").Find(&hosts).Error; err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("failed verification persisted a host: %+v", hosts)
	}
	if hosts[0].Status != "online" || hosts[0].LastCheckedAt == nil || hosts[0].Name != "local" {
		t.Fatalf("verified host: %+v", hosts[0])
	}
	if hosts[1].Status != "unknown" || hosts[1].LastCheckedAt != nil || hosts[1].LastError != "" {
		t.Fatalf("unverified host: %+v", hosts[1])
	}
}

func TestCreateHostVerificationRejectsFailedSSH(t *testing.T) {
	h, router := connectivityHandler(t)
	port := connectivitySSH(t, false)
	response := connectivityRequest(t, router, "/hosts", hostCreateRequest{Name: "fails", IP: "127.0.0.1", Port: port, Username: "root", Password: "private-password-marker", VerifyConnection: true})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("response %d: %s", response.Code, response.Body)
	}
	var count int64
	if err := h.db.Model(&models.Host{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 || strings.Contains(response.Body.String(), "private-password-marker") {
		t.Fatalf("rejected host count=%d, response=%s", count, response.Body)
	}
}

func TestHostConnectivityPersistsFailureAndRecovery(t *testing.T) {
	h, router := connectivityHandler(t)
	host := models.Host{Name: "target", IP: "127.0.0.1", Port: connectivitySSH(t, false), ConnectionType: "ssh", Username: "root", Password: "private-password-marker", Status: "online"}
	if err := h.db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	response := connectivityRequest(t, router, fmt.Sprintf("/hosts/%d/test", host.ID), nil)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("failure response %d: %s", response.Code, response.Body)
	}
	var stored models.Host
	if err := h.db.First(&stored, host.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "offline" || stored.LastError == "" || stored.LastCheckedAt == nil || strings.Contains(stored.LastError, "private-password-marker") {
		t.Fatalf("failure not safely persisted: %+v", stored)
	}
	checked := *stored.LastCheckedAt
	if err := h.db.Model(&models.Host{}).Where("id = ?", host.ID).Update("port", connectivitySSH(t, true)).Error; err != nil {
		t.Fatal(err)
	}
	response = connectivityRequest(t, router, fmt.Sprintf("/hosts/%d/test", host.ID), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("recovery response %d: %s", response.Code, response.Body)
	}
	if err := h.db.First(&stored, host.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "online" || stored.LastError != "" || stored.LastOnline == nil || stored.LastCheckedAt == nil || stored.LastCheckedAt.Before(checked) {
		t.Fatalf("recovery not persisted: %+v", stored)
	}
}

func TestHostProbeCancellationDoesNotBlockOtherCreates(t *testing.T) {
	h, router := connectivityHandler(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		close(accepted)
		_, _ = io.Copy(io.Discard, conn) // never send an SSH banner
	}()
	host := models.Host{Name: "stalled", IP: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Username: "root", Password: "secret", ConnectionType: "ssh", Status: "unknown"}
	if err := h.db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/hosts/%d/test", host.ID), nil).WithContext(ctx)
	probeResponse := httptest.NewRecorder()
	probeDone := make(chan struct{})
	go func() { defer close(probeDone); router.ServeHTTP(probeResponse, request) }()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not connect")
	}
	created := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		created <- connectivityRequest(t, router, "/hosts", hostCreateRequest{Name: "independent", IP: "127.0.0.2", ConnectionType: "local", VerifyConnection: true})
	}()
	select {
	case response := <-created:
		if response.Code != http.StatusCreated {
			t.Fatalf("independent create %d: %s", response.Code, response.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled probe blocked unrelated creation")
	}
	cancel()
	select {
	case <-probeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled probe did not return")
	}
	if probeResponse.Code != http.StatusBadGateway {
		t.Fatalf("canceled response %d", probeResponse.Code)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled probe left TCP transport alive")
	}
	var stored models.Host
	if err := h.db.First(&stored, host.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "offline" || stored.LastCheckedAt == nil || stored.LastOnline != nil {
		t.Fatalf("canceled probe state: %+v", stored)
	}
}
