package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testAPIKey is a fake, non-secret key used only to exercise header/log behavior.
// The real key must never appear in any test fixture or source file.
const testAPIKey = "test-devin-key-NOT-REAL"

func TestTriggerReview_PostsSessionWithBearerAndPrompt(t *testing.T) {
	t.Parallel()

	const prURL = "https://github.com/acme/widgets/pull/42"
	const headSHA = "deadbeefcafef00dba5eba11deadbeefcafef00d"

	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotCT     string
		gotPrompt string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		var req struct {
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		gotPrompt = req.Prompt
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"sess-123","url":"https://app.devin.ai/sessions/sess-123"}`)
	}))
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	sessionID, err := c.TriggerReview(context.Background(), testAPIKey, prURL, headSHA)
	if err != nil {
		t.Fatalf("TriggerReview() error = %v", err)
	}
	if sessionID != "sess-123" {
		t.Errorf("sessionID = %q, want sess-123", sessionID)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/sessions" {
		t.Errorf("path = %q, want /v1/sessions", gotPath)
	}
	if gotAuth != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q, want Bearer <key>", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if !strings.Contains(gotPrompt, prURL) {
		t.Errorf("prompt missing PR URL %q: %q", prURL, gotPrompt)
	}
	if !strings.Contains(gotPrompt, headSHA) {
		t.Errorf("prompt missing head SHA %q: %q", headSHA, gotPrompt)
	}
}

func TestTriggerReview_Non2xxIsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	_, err := c.TriggerReview(context.Background(), testAPIKey, "https://example/pr/1", "abc123")
	if err == nil {
		t.Fatal("expected error on non-2xx status, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention the status, got: %v", err)
	}
}

func TestTriggerReview_MissingSessionIDIsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"url":"https://app.devin.ai/sessions/x"}`)
	}))
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	if _, err := c.TriggerReview(context.Background(), testAPIKey, "https://example/pr/1", "abc"); err == nil {
		t.Fatal("expected error when session_id is missing, got nil")
	}
}

func TestTriggerReview_EmptyKeyIsError(t *testing.T) {
	t.Parallel()
	c := &Client{}
	if _, err := c.TriggerReview(context.Background(), "  ", "https://example/pr/1", "abc"); err == nil {
		t.Fatal("expected error for empty API key, got nil")
	}
}

// TestTriggerReview_KeyNeverLogged captures slog output across both the success
// and error paths and asserts the API key never appears in any log line.
func TestTriggerReview_KeyNeverLogged(t *testing.T) {
	// Not parallel: it swaps the process-global slog default logger.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"sess-9","url":"https://app.devin.ai/sessions/sess-9"}`)
	}))
	defer okSrv.Close()
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	defer errSrv.Close()

	okClient := &Client{HTTPClient: okSrv.Client(), BaseURL: okSrv.URL}
	if _, err := okClient.TriggerReview(context.Background(), testAPIKey, "https://example/pr/1", "abc"); err != nil {
		t.Fatalf("success path error = %v", err)
	}
	errClient := &Client{HTTPClient: errSrv.Client(), BaseURL: errSrv.URL}
	if _, err := errClient.TriggerReview(context.Background(), testAPIKey, "https://example/pr/1", "abc"); err == nil {
		t.Fatal("expected error from 401 path")
	}

	if strings.Contains(buf.String(), testAPIKey) {
		t.Fatalf("API key leaked into logs: %s", buf.String())
	}
}

// TestTriggerReview_ErrorNeverContainsKey asserts the key is not embedded in the
// returned error either (errors are logged by callers).
func TestTriggerReview_ErrorNeverContainsKey(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	_, err := c.TriggerReview(context.Background(), testAPIKey, "https://example/pr/1", "abc")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("API key leaked into error: %v", err)
	}
}
