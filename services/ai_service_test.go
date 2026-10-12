package services

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
)

func testAIService(client *http.Client) *AIService {
	return &AIService{newClient: func(timeout time.Duration) *http.Client { copy := *client; copy.Timeout = timeout; return &copy }, maxResponseBytes: maxAIResponseBytes}
}
func enabledTestConfig(baseURL string) models.AIConfig {
	return models.AIConfig{Provider: "test-provider", BaseURL: baseURL, Model: "test-model", APIKey: "sk-test-secret", Timeout: 2, Enabled: true}
}

func TestAIServiceChatOpenAICompatible(t *testing.T) {
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"returned-model","choices":[{"message":{"content":"建议检查告警规则"}}]}`)
	}))
	defer server.Close()
	result, err := testAIService(server.Client()).Chat(context.Background(), enabledTestConfig(server.URL+"/v1/"), "分析问题", "只读数据")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if requestPath != "/v1/chat/completions" {
		t.Fatalf("request path = %q", requestPath)
	}
	if result.Answer != "建议检查告警规则" || result.Model != "returned-model" || result.Provider != "test-provider" {
		t.Fatalf("unexpected result: %+v", result)
	}
}
func TestAIServicePrivateProviderRequiresExplicitOptIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local ok"}}]}`)
	}))
	defer server.Close()
	cfg := enabledTestConfig(server.URL)
	t.Setenv("PROMLIFECYCLE_ALLOW_PRIVATE_FETCH", "0")
	if _, err := NewAIService().Chat(context.Background(), cfg, "hello", ""); err == nil {
		t.Fatal("private provider was reachable without explicit opt-in")
	}
	t.Setenv("PROMLIFECYCLE_ALLOW_PRIVATE_FETCH", "1")
	result, err := NewAIService().Chat(context.Background(), cfg, "hello", "")
	if err != nil {
		t.Fatalf("explicitly allowed private provider: %v", err)
	}
	if result.Answer != "local ok" {
		t.Fatalf("answer = %q", result.Answer)
	}
}
func TestAIServiceDisabledDoesNotCallProvider(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	cfg := enabledTestConfig(server.URL)
	cfg.Enabled = false
	_, err := testAIService(server.Client()).Chat(context.Background(), cfg, "hello", "")
	if !errors.Is(err, ErrAIDisabled) {
		t.Fatalf("error = %v, want ErrAIDisabled", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider called %d times", calls.Load())
	}
}
func TestAIServiceTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"late"}}]}`)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	cfg := enabledTestConfig(server.URL)
	cfg.Timeout = 1
	_, err := testAIService(server.Client()).Chat(context.Background(), cfg, "hello", "")
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("error = %v, want timeout", err)
	}
}
func TestAIServiceRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 65)) }))
	defer server.Close()
	service := testAIService(server.Client())
	service.maxResponseBytes = 64
	_, err := service.Chat(context.Background(), enabledTestConfig(server.URL), "hello", "")
	if !errors.Is(err, ErrAIResponseTooBig) {
		t.Fatalf("error = %v, want ErrAIResponseTooBig", err)
	}
}
func TestAIServiceOpenAIErrorRedactsAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key sk-test-secret"}}`)
	}))
	defer server.Close()
	_, err := testAIService(server.Client()).Chat(context.Background(), enabledTestConfig(server.URL), "hello", "")
	if err == nil {
		t.Fatal("expected provider error")
	}
	var providerErr *AIProviderError
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error = %#v, want AIProviderError 401", err)
	}
	if strings.Contains(err.Error(), "sk-test-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("API key was not safely redacted: %v", err)
	}
}
func TestNormalizeAIBaseURL(t *testing.T) {
	got, err := NormalizeAIBaseURL(" https://example.com/v1/chat/completions/ ")
	if err != nil {
		t.Fatalf("NormalizeAIBaseURL: %v", err)
	}
	if got != "https://example.com/v1" {
		t.Fatalf("normalized = %q", got)
	}
	if _, err := NormalizeAIBaseURL("file:///etc/passwd"); err == nil {
		t.Fatal("file URL unexpectedly accepted")
	}
}

func TestParseAIAnalysisSeparatesFactsFromHypotheses(t *testing.T) {
	analysis := parseAIAnalysis(`{"facts":["Prometheus active"],"hypotheses":["规则可能过于敏感"],"risks":["告警风暴"],"steps":["检查表达式"],"verification":["查询 rules API"]}`)
	if len(analysis.Facts) != 1 || len(analysis.Hypotheses) != 1 || len(analysis.Verification) != 1 {
		t.Fatalf("unexpected structured analysis: %#v", analysis)
	}
	fallback := parseAIAnalysis("普通文本回答")
	if len(fallback.Facts) != 0 || len(fallback.Hypotheses) != 1 || fallback.Hypotheses[0] != "普通文本回答" {
		t.Fatalf("unstructured response was promoted to facts: %#v", fallback)
	}
}
