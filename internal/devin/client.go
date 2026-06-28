// Package devin is a tiny HTTP client for the Devin API. The post-PR review loop
// uses it to EXPLICITLY (re-)trigger a Devin review of a PR head, because Devin's
// auto-review is rate-limited / pausable (empirically it has failed to auto-review
// a PR mid-loop) and its CLI is TTY-only — unusable from the headless daemon. Only
// the one endpoint the loop needs (POST /v1/sessions) is implemented.
//
// SECURITY: the Devin API key is a secret. It is read from the environment or a
// trust-gated key file (see ResolveAPIKey) and sent only in the Authorization
// header of the request. It is NEVER logged, never put in an error message, and
// never written to disk by this package. Logging on success is limited to the
// returned session_id and url, which are not secret.
package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// DefaultBaseURL is the Devin API origin. It is overridable per-Client (pointed
// at an httptest server in tests).
const DefaultBaseURL = "https://api.devin.ai"

// maxResponseBytes bounds how much of the response body is read, so a hostile or
// runaway endpoint cannot exhaust memory.
const maxResponseBytes = 1 << 20 // 1 MiB

// Client is a minimal Devin API client. The zero value is usable: it falls back
// to http.DefaultClient and DefaultBaseURL. Set HTTPClient and/or BaseURL to
// override (tests point BaseURL at an httptest server).
type Client struct {
	HTTPClient *http.Client
	BaseURL    string
}

// sessionRequest is the POST /v1/sessions body. Only the prompt is sent.
type sessionRequest struct {
	Prompt string `json:"prompt"`
}

// sessionResponse is the subset of the POST /v1/sessions response we consume.
type sessionResponse struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
}

// TriggerReview asks Devin to review prURL at headSHA by creating a session via
// POST {BaseURL}/v1/sessions with an `Authorization: Bearer <apiKey>` header and
// a JSON body whose prompt instructs Devin to review the PR for bugs / security /
// correctness, post inline comments, and NOT modify code or open PRs. It returns
// the created session_id.
//
// A non-2xx status is treated as an error. The apiKey is sent only in the
// Authorization header and is never logged or included in any returned error.
func (c *Client) TriggerReview(ctx context.Context, apiKey, prURL, headSHA string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", fmt.Errorf("devin: empty API key")
	}

	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	body, err := json.Marshal(sessionRequest{Prompt: reviewPrompt(prURL, headSHA)})
	if err != nil {
		return "", fmt.Errorf("devin: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("devin: new request: %w", err)
	}
	// SECURITY: the key lives only in this header for the duration of the request.
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("devin: post session: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The response body never contains the request's Authorization header, so a
		// bounded snippet is safe to surface for debugging (e.g. a rate-limit note).
		return "", fmt.Errorf("devin: session create returned %s: %s", resp.Status, snippet(respBody))
	}

	var parsed sessionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("devin: decode response: %w", err)
	}
	if strings.TrimSpace(parsed.SessionID) == "" {
		return "", fmt.Errorf("devin: response missing session_id")
	}

	// Log only non-secret identifiers on success. NEVER log apiKey.
	slog.Info("devin: triggered review session", "session_id", parsed.SessionID, "url", parsed.URL)
	return parsed.SessionID, nil
}

// reviewPrompt builds the session prompt. It references the PR URL and head SHA
// explicitly (so Devin reviews the exact commit) and constrains Devin to a
// review-only role: inline comments, no code changes, no new PRs.
func reviewPrompt(prURL, headSHA string) string {
	return fmt.Sprintf(
		"Please review the pull request at %s at head commit %s. "+
			"Carefully look for bugs, security vulnerabilities, and correctness issues, "+
			"and post your findings as inline review comments on the pull request. "+
			"Do NOT modify any code, do NOT push commits, and do NOT open any pull requests — review only.",
		prURL, headSHA)
}

// snippet returns a bounded, single-line view of a response body for error
// messages. It carries no secret (the key is request-only).
func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
