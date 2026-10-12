package winrm

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type fileTransfer struct {
	path      string
	expiresAt time.Time
	consumed  atomic.Bool
	done      chan struct{}
}

var fileTransfers sync.Map

func registerFileTransfer(path string, ttl time.Duration) (string, *fileTransfer, func(), error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, nil, os.ErrInvalid
	}
	var tokenBytes [32]byte
	if _, err = rand.Read(tokenBytes[:]); err != nil {
		return "", nil, nil, err
	}
	token := hex.EncodeToString(tokenBytes[:])
	transfer := &fileTransfer{path: path, expiresAt: time.Now().Add(ttl), done: make(chan struct{})}
	fileTransfers.Store(token, transfer)
	cleanup := func() { fileTransfers.Delete(token) }
	return token, transfer, cleanup, nil
}

// ServeFileTransfer serves a short-lived, single-use deployment file. The
// unguessable token is delivered only inside the encrypted WinRM session.
func ServeFileTransfer(response http.ResponseWriter, request *http.Request, token string) {
	value, ok := fileTransfers.Load(token)
	if !ok {
		http.NotFound(response, request)
		return
	}
	transfer := value.(*fileTransfer)
	if time.Now().After(transfer.expiresAt) {
		fileTransfers.Delete(token)
		http.NotFound(response, request)
		return
	}
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !transfer.consumed.CompareAndSwap(false, true) {
		response.WriteHeader(http.StatusGone)
		return
	}
	file, err := os.Open(transfer.path)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(transfer.path)+`"`)
	http.ServeContent(response, request, filepath.Base(transfer.path), info.ModTime(), file)
	close(transfer.done)
}
