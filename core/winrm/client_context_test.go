package winrm

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestWinRMContextCancelsAuthenticationAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "encrypted-auth"
		if fallback {
			name = "ntlm-fallback"
		}
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			closed := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				if requests.Add(1) == 1 && fallback {
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				select {
				case <-request.Context().Done():
					closed <- struct{}{}
				case <-time.After(4 * time.Second):
				}
			}))
			defer server.Close()
			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			host, portText, err := net.SplitHostPort(endpoint.Host)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			client, err := NewClientWithOptions(Options{Host: host, Port: port, Scheme: "http", Username: "root", Password: "secret", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if _, err := client.RunPS(ctx, "Write-Output ok"); err == nil {
				t.Fatal("stalled WinRM peer reported success")
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("WinRM escaped total deadline")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("canceled WinRM HTTP connection stayed open")
			}
			if fallback && requests.Load() != 2 {
				t.Fatalf("fallback was not exercised: requests=%d", requests.Load())
			}
		})
	}
}
