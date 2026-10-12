package winrm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileTransferIsSingleUseAndMethodBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.msi")
	content := []byte("msi-package")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	token, transfer, cleanup, err := registerFileTransfer(path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	post := httptest.NewRecorder()
	ServeFileTransfer(post, httptest.NewRequest(http.MethodPost, "/"+token, nil), token)
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", post.Code)
	}

	first := httptest.NewRecorder()
	ServeFileTransfer(first, httptest.NewRequest(http.MethodGet, "/"+token, nil), token)
	if first.Code != http.StatusOK || first.Body.String() != string(content) {
		t.Fatalf("first GET status=%d body=%q", first.Code, first.Body.String())
	}
	select {
	case <-transfer.done:
	default:
		t.Fatal("successful transfer did not signal completion")
	}

	second := httptest.NewRecorder()
	ServeFileTransfer(second, httptest.NewRequest(http.MethodGet, "/"+token, nil), token)
	if second.Code != http.StatusGone {
		t.Fatalf("second GET status=%d", second.Code)
	}
}

func TestExpiredFileTransferIsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.msi")
	if err := os.WriteFile(path, []byte("msi-package"), 0600); err != nil {
		t.Fatal(err)
	}
	token, _, cleanup, err := registerFileTransfer(path, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	response := httptest.NewRecorder()
	ServeFileTransfer(response, httptest.NewRequest(http.MethodGet, "/"+token, nil), token)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expired GET status=%d", response.Code)
	}
}
