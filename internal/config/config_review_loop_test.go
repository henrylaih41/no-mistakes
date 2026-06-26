package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMerge_ReviewLoopDefaults(t *testing.T) {
	cfg := Merge(&GlobalConfig{}, &RepoConfig{})

	if cfg.ReviewLoop.Enabled {
		t.Errorf("ReviewLoop.Enabled = true, want false (off by default)")
	}
	if cfg.ReviewLoop.BotLogin != DefaultReviewLoopBotLogin {
		t.Errorf("ReviewLoop.BotLogin = %q, want %q", cfg.ReviewLoop.BotLogin, DefaultReviewLoopBotLogin)
	}
	if cfg.ReviewLoop.MaxRounds != 3 {
		t.Errorf("ReviewLoop.MaxRounds = %d, want 3", cfg.ReviewLoop.MaxRounds)
	}
	if !cfg.ReviewLoop.FailOpen {
		t.Errorf("ReviewLoop.FailOpen = false, want true (silent reviewer must not block)")
	}
}

func TestMerge_ReviewLoopFromGlobal(t *testing.T) {
	enabled := true
	failOpen := false
	login := "my-bot[bot]"
	rounds := 5
	global := &GlobalConfig{
		ReviewLoop: ReviewLoopRaw{
			Enabled:   &enabled,
			BotLogin:  &login,
			MaxRounds: &rounds,
			FailOpen:  &failOpen,
		},
	}

	cfg := Merge(global, &RepoConfig{})

	if !cfg.ReviewLoop.Enabled {
		t.Errorf("ReviewLoop.Enabled = false, want true from global")
	}
	if cfg.ReviewLoop.BotLogin != "my-bot[bot]" {
		t.Errorf("ReviewLoop.BotLogin = %q, want my-bot[bot]", cfg.ReviewLoop.BotLogin)
	}
	if cfg.ReviewLoop.MaxRounds != 5 {
		t.Errorf("ReviewLoop.MaxRounds = %d, want 5", cfg.ReviewLoop.MaxRounds)
	}
	if cfg.ReviewLoop.FailOpen {
		t.Errorf("ReviewLoop.FailOpen = true, want false from global")
	}
}

func TestMerge_ReviewLoopRepoOverridesGlobalPerField(t *testing.T) {
	globalEnabled := true
	globalRounds := 9
	global := &GlobalConfig{
		ReviewLoop: ReviewLoopRaw{
			Enabled:   &globalEnabled,
			MaxRounds: &globalRounds,
		},
	}
	repoRounds := 1
	repo := &RepoConfig{
		ReviewLoop: ReviewLoopRaw{
			MaxRounds: &repoRounds,
		},
	}

	cfg := Merge(global, repo)

	// Repo overrides only max_rounds; enabled survives from global; bot_login and
	// fail_open keep their defaults.
	if !cfg.ReviewLoop.Enabled {
		t.Errorf("ReviewLoop.Enabled = false, want true (from global, not overridden by repo)")
	}
	if cfg.ReviewLoop.MaxRounds != 1 {
		t.Errorf("ReviewLoop.MaxRounds = %d, want 1 (repo override)", cfg.ReviewLoop.MaxRounds)
	}
	if cfg.ReviewLoop.BotLogin != DefaultReviewLoopBotLogin {
		t.Errorf("ReviewLoop.BotLogin = %q, want default", cfg.ReviewLoop.BotLogin)
	}
	if !cfg.ReviewLoop.FailOpen {
		t.Errorf("ReviewLoop.FailOpen = false, want default true")
	}
}

func TestLoadGlobal_ReviewLoopParsesUnderStrictKnownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent: claude
review_loop:
  enabled: true
  bot_login: "custom-bot[bot]"
  max_rounds: 2
  fail_open: false
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal() error = %v", err)
	}
	if cfg.ReviewLoop.Enabled == nil || !*cfg.ReviewLoop.Enabled {
		t.Errorf("ReviewLoop.Enabled = %v, want true", cfg.ReviewLoop.Enabled)
	}
	if cfg.ReviewLoop.BotLogin == nil || *cfg.ReviewLoop.BotLogin != "custom-bot[bot]" {
		t.Errorf("ReviewLoop.BotLogin = %v, want custom-bot[bot]", cfg.ReviewLoop.BotLogin)
	}
	if cfg.ReviewLoop.MaxRounds == nil || *cfg.ReviewLoop.MaxRounds != 2 {
		t.Errorf("ReviewLoop.MaxRounds = %v, want 2", cfg.ReviewLoop.MaxRounds)
	}
	if cfg.ReviewLoop.FailOpen == nil || *cfg.ReviewLoop.FailOpen {
		t.Errorf("ReviewLoop.FailOpen = %v, want false", cfg.ReviewLoop.FailOpen)
	}
}

func TestLoadGlobal_ReviewLoopUnknownKeyTripsKnownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `review_loop:
  enabled: true
  bogus: 1
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("expected error: unknown key under review_loop must trip KnownFields(true)")
	}
}

func TestLoadGlobal_ReviewLoopRejectsNegativeMaxRounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `review_loop:
  max_rounds: -1
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for negative max_rounds")
	}
	if !strings.Contains(err.Error(), "max_rounds") {
		t.Errorf("expected error to mention max_rounds, got: %v", err)
	}
}

func TestLoadRepo_ReviewLoopParses(t *testing.T) {
	dir := t.TempDir()
	repoYAML := "review_loop:\n  enabled: true\n  bot_login: \"repo-bot[bot]\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("LoadRepo() error = %v", err)
	}
	if cfg.ReviewLoop.Enabled == nil || !*cfg.ReviewLoop.Enabled {
		t.Errorf("ReviewLoop.Enabled = %v, want true", cfg.ReviewLoop.Enabled)
	}
	if cfg.ReviewLoop.BotLogin == nil || *cfg.ReviewLoop.BotLogin != "repo-bot[bot]" {
		t.Errorf("ReviewLoop.BotLogin = %v, want repo-bot[bot]", cfg.ReviewLoop.BotLogin)
	}
}

func TestLoadRepoFromBytes_ReviewLoopRejectsNegativeMaxRounds(t *testing.T) {
	data := []byte("review_loop:\n  max_rounds: -3\n")
	_, err := LoadRepoFromBytes(data)
	if err == nil {
		t.Fatal("expected error: repo-level negative max_rounds must be rejected")
	}
	if !strings.Contains(err.Error(), "max_rounds") {
		t.Errorf("expected error to mention max_rounds, got: %v", err)
	}
}

// TestEffectiveRepoConfig_StripsPushedReviewLoop proves the trust gate: review_loop
// is execution-affecting (it gates CI, names the bot login whose comments become
// fix-prompt content, and bounds fix rounds), so a block pushed on a feature
// branch must never win - the effective loop comes from the trusted
// default-branch copy, exactly like the review panel.
func TestEffectiveRepoConfig_StripsPushedReviewLoop(t *testing.T) {
	pushedEnabled := true
	pushedLogin := "attacker-bot[bot]"
	pushedRounds := 99
	pushed := &RepoConfig{
		ReviewLoop: ReviewLoopRaw{Enabled: &pushedEnabled, BotLogin: &pushedLogin, MaxRounds: &pushedRounds},
	}
	trustedEnabled := true
	trustedLogin := "devin-ai-integration[bot]"
	trusted := &RepoConfig{
		ReviewLoop: ReviewLoopRaw{Enabled: &trustedEnabled, BotLogin: &trustedLogin},
	}

	got := EffectiveRepoConfig(pushed, trusted, false)

	if got.ReviewLoop.BotLogin == nil || *got.ReviewLoop.BotLogin != "devin-ai-integration[bot]" {
		t.Errorf("ReviewLoop.BotLogin = %v, want trusted devin login, not pushed attacker-bot", got.ReviewLoop.BotLogin)
	}
	if got.ReviewLoop.MaxRounds != nil {
		t.Errorf("ReviewLoop.MaxRounds = %v, want trusted (nil), not pushed 99", got.ReviewLoop.MaxRounds)
	}
	// The pushed config must not be mutated.
	if pushed.ReviewLoop.BotLogin == nil || *pushed.ReviewLoop.BotLogin != "attacker-bot[bot]" {
		t.Errorf("pushed config was mutated: bot_login = %v", pushed.ReviewLoop.BotLogin)
	}
}

// TestEffectiveRepoConfig_NoTrustedZeroesReviewLoop proves that a pushed review_loop
// with no trusted copy and no opt-in is forced off, blocking the supply-chain
// vector for repos that ship .no-mistakes.yaml only on feature branches.
func TestEffectiveRepoConfig_NoTrustedZeroesReviewLoop(t *testing.T) {
	enabled := true
	login := "pushed-bot[bot]"
	pushed := &RepoConfig{
		ReviewLoop: ReviewLoopRaw{Enabled: &enabled, BotLogin: &login},
	}

	got := EffectiveRepoConfig(pushed, nil, false)

	if got.ReviewLoop != (ReviewLoopRaw{}) {
		t.Errorf("ReviewLoop = %+v, want zero value (stripped, no trusted copy)", got.ReviewLoop)
	}
}

// TestEffectiveRepoConfig_OptInHonorsPushedReviewLoop proves the maintainer opt-in
// (allow_repo_commands on the trusted copy) honors the pushed review_loop wholesale.
func TestEffectiveRepoConfig_OptInHonorsPushedReviewLoop(t *testing.T) {
	enabled := true
	login := "pushed-bot[bot]"
	pushed := &RepoConfig{
		ReviewLoop: ReviewLoopRaw{Enabled: &enabled, BotLogin: &login},
	}
	trustedLogin := "devin-ai-integration[bot]"
	trusted := &RepoConfig{
		ReviewLoop: ReviewLoopRaw{BotLogin: &trustedLogin},
	}

	got := EffectiveRepoConfig(pushed, trusted, true)

	if got.ReviewLoop.BotLogin == nil || *got.ReviewLoop.BotLogin != "pushed-bot[bot]" {
		t.Errorf("ReviewLoop.BotLogin = %v, want pushed-bot under opt-in", got.ReviewLoop.BotLogin)
	}
}
