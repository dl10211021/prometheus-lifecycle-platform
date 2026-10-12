package services

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAIProviderNeverReceivesEmbeddedConfigCredentials(t *testing.T) {
	captured := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		captured <- string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"safe answer"}}]}`)
	}))
	defer server.Close()
	for _, input := range []string{
		`{"data":{"content_type":"yaml","content":"authorization:\n  credentials: CANARY_AUTH\nunknown_extension:\n  name: CANARY_UNKNOWN\n"},"diff":"password: CANARY_DIFF"}`,
		`unknown-provider-context CANARY_UNSTRUCTURED`,
	} {
		_, err := testAIService(server.Client()).Chat(context.Background(), enabledTestConfig(server.URL), "analyze", input)
		if err != nil {
			t.Fatal(err)
		}
		if payload := <-captured; strings.Contains(payload, "CANARY") {
			t.Fatalf("provider received credentials: %s", payload)
		}
	}
}
