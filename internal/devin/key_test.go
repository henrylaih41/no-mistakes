package devin

import (
	"os"
	"path/filepath"
	"testing"
)

// keyResolutionFakeKey is a fake, non-secret value. A real key must never appear
// in a test file.
const keyResolutionFakeKey = "resolved-fake-key"

func TestResolveAPIKey_EnvPrecedence(t *testing.T) {
	// Env takes precedence even when a key file is present and readable.
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "api_key")
	if err := os.WriteFile(keyFile, []byte("file-key-should-be-ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAPIKey, "  "+keyResolutionFakeKey+"\n")

	if got := ResolveAPIKey(keyFile); got != keyResolutionFakeKey {
		t.Errorf("ResolveAPIKey() = %q, want env key %q (trimmed)", got, keyResolutionFakeKey)
	}
}

func TestResolveAPIKey_FileFallbackTrimmed(t *testing.T) {
	t.Setenv(EnvAPIKey, "") // env empty -> file fallback
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "api_key")
	if err := os.WriteFile(keyFile, []byte("\n  "+keyResolutionFakeKey+"  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := ResolveAPIKey(keyFile); got != keyResolutionFakeKey {
		t.Errorf("ResolveAPIKey() = %q, want trimmed file key %q", got, keyResolutionFakeKey)
	}
}

func TestResolveAPIKey_TildeExpansion(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir honors HOME on unix
	cfgDir := filepath.Join(home, ".config", "devin")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "api_key"), []byte(keyResolutionFakeKey), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := ResolveAPIKey("~/.config/devin/api_key"); got != keyResolutionFakeKey {
		t.Errorf("ResolveAPIKey() = %q, want %q after ~ expansion", got, keyResolutionFakeKey)
	}
}

func TestResolveAPIKey_EmptyKeyFileUsesDefault(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".config", "devin")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "api_key"), []byte(keyResolutionFakeKey), 0o600); err != nil {
		t.Fatal(err)
	}

	// An empty keyFile falls back to DefaultAPIKeyFile (~/.config/devin/api_key).
	if got := ResolveAPIKey(""); got != keyResolutionFakeKey {
		t.Errorf("ResolveAPIKey(\"\") = %q, want %q via DefaultAPIKeyFile", got, keyResolutionFakeKey)
	}
}

func TestResolveAPIKey_BothAbsentReturnsEmpty(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	if got := ResolveAPIKey(missing); got != "" {
		t.Errorf("ResolveAPIKey() = %q, want \"\" when env empty and file absent", got)
	}
}

func TestResolveAPIKey_WhitespaceEnvFallsThroughToFile(t *testing.T) {
	// A whitespace-only env value is treated as empty, so the file is used.
	t.Setenv(EnvAPIKey, "   \t\n")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "api_key")
	if err := os.WriteFile(keyFile, []byte(keyResolutionFakeKey), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := ResolveAPIKey(keyFile); got != keyResolutionFakeKey {
		t.Errorf("ResolveAPIKey() = %q, want file key (whitespace env ignored)", got)
	}
}

func TestResolveReviewAPIKey_EnvPrecedence(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "review_api_key")
	if err := os.WriteFile(keyFile, []byte("file-token-should-be-ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvReviewAPIKey, "  "+keyResolutionFakeKey+"\n")

	if got := ResolveReviewAPIKey(keyFile); got != keyResolutionFakeKey {
		t.Errorf("ResolveReviewAPIKey() = %q, want trimmed env token %q", got, keyResolutionFakeKey)
	}
}

func TestResolveReviewAPIKey_FileFallbackTrimmed(t *testing.T) {
	t.Setenv(EnvReviewAPIKey, "")
	keyFile := filepath.Join(t.TempDir(), "review_api_key")
	if err := os.WriteFile(keyFile, []byte("\n  "+keyResolutionFakeKey+"  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := ResolveReviewAPIKey(keyFile); got != keyResolutionFakeKey {
		t.Errorf("ResolveReviewAPIKey() = %q, want trimmed file token %q", got, keyResolutionFakeKey)
	}
}

func TestResolveReviewAPIKey_BothAbsentReturnsEmpty(t *testing.T) {
	t.Setenv(EnvReviewAPIKey, "")
	missing := filepath.Join(t.TempDir(), "absent_review_key")

	if got := ResolveReviewAPIKey(missing); got != "" {
		t.Errorf("ResolveReviewAPIKey() = %q, want \"\" when env empty and file absent", got)
	}
}
