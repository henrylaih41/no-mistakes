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
	if !cfg.ReviewLoop.ReplyOnFix {
		t.Errorf("ReviewLoop.ReplyOnFix = false, want true (acknowledge addressed findings by default)")
	}
	if !cfg.ReviewLoop.Retrigger {
		t.Errorf("ReviewLoop.Retrigger = false, want true (explicit Devin re-trigger on by default)")
	}
	if cfg.ReviewLoop.DevinAPIKeyFile != DefaultDevinAPIKeyFile {
		t.Errorf("ReviewLoop.DevinAPIKeyFile = %q, want %q", cfg.ReviewLoop.DevinAPIKeyFile, DefaultDevinAPIKeyFile)
	}
}

func TestMerge_ReviewLoopRetriggerAndKeyFileOverrides(t *testing.T) {
	retriggerOff := false
	keyFile := "/etc/secrets/devin_key"
	global := &GlobalConfig{
		ReviewLoop: ReviewLoopRaw{Retrigger: &retriggerOff, DevinAPIKeyFile: &keyFile},
	}

	cfg := Merge(global, &RepoConfig{})

	if cfg.ReviewLoop.Retrigger {
		t.Errorf("ReviewLoop.Retrigger = true, want false from global override")
	}
	if cfg.ReviewLoop.DevinAPIKeyFile != keyFile {
		t.Errorf("ReviewLoop.DevinAPIKeyFile = %q, want %q from global override", cfg.ReviewLoop.DevinAPIKeyFile, keyFile)
	}
}

// TestMerge_ReviewLoopBlankKeyFileKeepsDefault asserts a whitespace-only
// devin_api_key_file override is ignored so the default path survives.
func TestMerge_ReviewLoopBlankKeyFileKeepsDefault(t *testing.T) {
	blank := "   "
	global := &GlobalConfig{ReviewLoop: ReviewLoopRaw{DevinAPIKeyFile: &blank}}

	cfg := Merge(global, &RepoConfig{})

	if cfg.ReviewLoop.DevinAPIKeyFile != DefaultDevinAPIKeyFile {
		t.Errorf("ReviewLoop.DevinAPIKeyFile = %q, want default %q (blank override ignored)", cfg.ReviewLoop.DevinAPIKeyFile, DefaultDevinAPIKeyFile)
	}
}

// TestMerge_ReviewLoopRepoClearsDevinOrgID asserts a repo-level devin_org_id: ""
// clears a globally-set org id, since DevinOrgID has empty-means-disabled
// semantics (it is the switch that selects the Review API). This lets one repo opt
// back onto the legacy /v1/sessions path without disabling retrigger entirely.
func TestMerge_ReviewLoopRepoClearsDevinOrgID(t *testing.T) {
	globalOrg := "org-42"
	emptyOrg := ""
	global := &GlobalConfig{ReviewLoop: ReviewLoopRaw{DevinOrgID: &globalOrg}}
	repo := &RepoConfig{ReviewLoop: ReviewLoopRaw{DevinOrgID: &emptyOrg}}

	cfg := Merge(global, repo)

	if cfg.ReviewLoop.DevinOrgID != "" {
		t.Errorf("ReviewLoop.DevinOrgID = %q, want \"\" (repo override clears global)", cfg.ReviewLoop.DevinOrgID)
	}
}

func TestMerge_ReviewLoopReplyOnFixOverride(t *testing.T) {
	replyOff := false
	global := &GlobalConfig{
		ReviewLoop: ReviewLoopRaw{ReplyOnFix: &replyOff},
	}

	cfg := Merge(global, &RepoConfig{})

	if cfg.ReviewLoop.ReplyOnFix {
		t.Errorf("ReviewLoop.ReplyOnFix = true, want false from global override")
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

// TestEffectiveRepoConfig_StripsPushedDevinKeyFile proves the security
// requirement that a pushed branch cannot redirect devin_api_key_file to read an
// arbitrary file: the effective key-file path comes from the trusted copy.
func TestEffectiveRepoConfig_StripsPushedDevinKeyFile(t *testing.T) {
	pushedKeyFile := "/etc/passwd"
	pushed := &RepoConfig{
		ReviewLoop: ReviewLoopRaw{DevinAPIKeyFile: &pushedKeyFile},
	}
	trustedKeyFile := "~/.config/devin/api_key"
	trusted := &RepoConfig{
		ReviewLoop: ReviewLoopRaw{DevinAPIKeyFile: &trustedKeyFile},
	}

	got := EffectiveRepoConfig(pushed, trusted, false)

	if got.ReviewLoop.DevinAPIKeyFile == nil || *got.ReviewLoop.DevinAPIKeyFile != trustedKeyFile {
		t.Errorf("DevinAPIKeyFile = %v, want trusted %q, not pushed /etc/passwd", got.ReviewLoop.DevinAPIKeyFile, trustedKeyFile)
	}
}

func TestLoadGlobal_ReviewLoopRetriggerAndKeyFileParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `review_loop:
  enabled: true
  retrigger: false
  devin_api_key_file: "/secrets/devin"
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal() error = %v", err)
	}
	if cfg.ReviewLoop.Retrigger == nil || *cfg.ReviewLoop.Retrigger {
		t.Errorf("ReviewLoop.Retrigger = %v, want false", cfg.ReviewLoop.Retrigger)
	}
	if cfg.ReviewLoop.DevinAPIKeyFile == nil || *cfg.ReviewLoop.DevinAPIKeyFile != "/secrets/devin" {
		t.Errorf("ReviewLoop.DevinAPIKeyFile = %v, want /secrets/devin", cfg.ReviewLoop.DevinAPIKeyFile)
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
