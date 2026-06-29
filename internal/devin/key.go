package devin

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvAPIKey is the environment variable that, when non-empty, takes precedence
// as the Devin API key source.
const EnvAPIKey = "DEVIN_API_KEY"

// DefaultAPIKeyFile is the default path read for the API key when the
// environment variable is unset. A leading ~ is expanded to the user home dir.
//
// SECURITY: in the daemon this path comes from the trust-gated ReviewLoop config
// (honored only from the trusted default-branch copy), so an untrusted PR branch
// cannot redirect it to read/exfiltrate an arbitrary file. ResolveAPIKey itself
// performs no trust check; callers must pass a trusted path.
const DefaultAPIKeyFile = "~/.config/devin/api_key"

// ResolveAPIKey resolves the Devin API key, preferring the DEVIN_API_KEY
// environment variable. When that is empty it reads keyFile (an empty keyFile
// falls back to DefaultAPIKeyFile; a leading ~ is expanded via os.UserHomeDir)
// and returns its TrimSpace'd contents. It returns "" when neither source yields
// a key, so the caller can SKIP the trigger (best-effort).
//
// SECURITY: the resolved key is returned to the caller and never logged here.
func ResolveAPIKey(keyFile string) string {
	if key := strings.TrimSpace(os.Getenv(EnvAPIKey)); key != "" {
		return key
	}

	path := strings.TrimSpace(keyFile)
	if path == "" {
		path = DefaultAPIKeyFile
	}
	expanded, err := expandHome(path)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// EnvReviewAPIKey is the environment variable that, when non-empty, takes
// precedence as the Devin Review API token source.
const EnvReviewAPIKey = "DEVIN_REVIEW_API_KEY"

// DefaultReviewAPIKeyFile is the default path read for the Devin Review API token
// (a cog_-prefixed service-user token, distinct from the /v1/sessions key) when
// EnvReviewAPIKey is unset. A leading ~ is expanded to the user home dir.
//
// SECURITY: like DefaultAPIKeyFile, in the daemon this path comes from the
// trust-gated ReviewLoop config (honored only from the trusted default-branch
// copy), so an untrusted PR branch cannot redirect it. Callers pass a trusted path.
const DefaultReviewAPIKeyFile = "~/.config/devin/review_api_key"

// ResolveReviewAPIKey resolves the Devin Review API token, preferring the
// DEVIN_REVIEW_API_KEY environment variable. When that is empty it reads keyFile
// (an empty keyFile falls back to DefaultReviewAPIKeyFile; a leading ~ is
// expanded) and returns its TrimSpace'd contents. It returns "" when neither
// source yields a token, so the caller can fall back to the legacy /v1/sessions
// trigger.
//
// SECURITY: the resolved token is returned to the caller and never logged here.
func ResolveReviewAPIKey(keyFile string) string {
	if key := strings.TrimSpace(os.Getenv(EnvReviewAPIKey)); key != "" {
		return key
	}

	path := strings.TrimSpace(keyFile)
	if path == "" {
		path = DefaultReviewAPIKeyFile
	}
	expanded, err := expandHome(path)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// expandHome expands a leading ~ (alone, or as a ~/ prefix) in path to the
// user's home directory. Any other path is returned unchanged.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[len("~/"):]), nil
}
