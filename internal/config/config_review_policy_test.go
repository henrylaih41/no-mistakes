package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cutoverGlobalYAML is the shape of the machine's global config after the
// v1.84 cutover: review policy under review:, reviewer routing under
// review_agents, and machine-global design context.
const cutoverGlobalYAML = `agent: claude
ci_timeout: "4h"
log_level: info
agent_args_override:
  claude:
    - --model
    - claude-opus-5-5
    - --effort
    - high
  codex:
    - -m
    - gpt-6-astra
    - -c
    - model_reasoning_effort="xhigh"
  grok:
    - -m
    - grok-4.6
auto_fix:
  rebase: 3
  lint: 3
  test: 3
  review: 0
  document: 3
  ci: 3
intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
review:
  max_fix_rounds: 3
  fix_round_min_severity: warning
review_agents:
  reviewer:
    agent: codex
design_context:
  files:
    - /Users/example/.agents/QUALITY.md
    - /Users/example/.agents/REVIEW-QUALITY.md
`

// capturedEvalGlobalYAML is the shape every auto-captured eval case pins:
// agent keys stripped, the retired review_loop block, and (in most cases) the
// retired review.max_parallel panel width.
const capturedEvalGlobalYAML = `auto_fix:
  ci: 3
  document: 3
  lint: 3
  rebase: 3
  review: 0
  test: 3
ci_timeout: 4h
design_context:
  files:
    - /Users/example/.agents/QUALITY.md
intent:
  enabled: true
  slack_days: 3
  threshold: 0.2
log_level: info
review:
  max_fix_rounds: 3
  max_parallel: 2
review_loop:
  devin_org_id: org_example
  enabled: false
`

// exampleHome stands in for the /Users/example home in the YAML above, as an
// absolute path on the host running the test: design_context.files entries
// must be absolute, and "/Users/..." is not absolute on Windows.
func exampleHome() string {
	if runtime.GOOS == "windows" {
		return `C:\Users\example`
	}
	return "/Users/example"
}

func onThisHost(yaml string) []byte {
	return []byte(strings.ReplaceAll(yaml, "/Users/example", exampleHome()))
}

func TestCutoverGlobalConfigLoadsAndMerges(t *testing.T) {
	global, err := LoadGlobalFromBytes(onThisHost(cutoverGlobalYAML))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	merged := Merge(global, &RepoConfig{})
	if merged.Review.MaxFixRounds != 3 || merged.Review.FixRoundMinSeverity != "warning" {
		t.Fatalf("review = %+v, want max_fix_rounds 3 and warning", merged.Review)
	}
	if got := merged.ReviewAgents["reviewer"].Agent; got != "codex" {
		t.Fatalf("review_agents.reviewer.agent = %q, want codex", got)
	}
	want := []string{filepath.Join(exampleHome(), ".agents", "QUALITY.md"), filepath.Join(exampleHome(), ".agents", "REVIEW-QUALITY.md")}
	if strings.Join(merged.DesignContext.GlobalFiles, ",") != strings.Join(want, ",") {
		t.Fatalf("design_context global files = %v, want %v", merged.DesignContext.GlobalFiles, want)
	}
}

func TestCapturedEvalGlobalConfigStillLoads(t *testing.T) {
	global, err := LoadGlobalFromBytes(onThisHost(capturedEvalGlobalYAML))
	if err != nil {
		t.Fatalf("captured eval global config must load: %v", err)
	}
	merged := Merge(global, &RepoConfig{})
	if merged.Review.MaxFixRounds != 3 {
		t.Fatalf("max_fix_rounds = %d, want 3", merged.Review.MaxFixRounds)
	}
	without, err := LoadGlobalFromBytes(onThisHost(strings.Replace(capturedEvalGlobalYAML, "  max_parallel: 2\n", "", 1)))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes without max_parallel: %v", err)
	}
	if got, want := Merge(without, &RepoConfig{}).Review, merged.Review; got.MaxFixRounds != want.MaxFixRounds || got.FixRoundMinSeverity != want.FixRoundMinSeverity {
		t.Fatalf("retired max_parallel changed the review policy: with = %+v, without = %+v", want, got)
	}
}

func TestRetiredReviewLoopRejectsEnabled(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("review_loop:\n  enabled: true\n"))
	if err == nil || !strings.Contains(err.Error(), "review_loop.enabled must be false") {
		t.Fatalf("error = %v, want review_loop.enabled rejection", err)
	}
}

func TestRetiredReviewLoopRejectsUnknownSubkey(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("review_loop:\n  something_else: true\n")); err == nil {
		t.Fatal("unknown review_loop subkey must be rejected like any unknown field")
	}
}

func TestGlobalReviewAgentIsAParseError(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("review:\n  agent: codex\n"))
	if err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("error = %v, want review.agent rejected by the strict decoder", err)
	}
}

func TestGlobalReviewRejectsDroppedPanelKeys(t *testing.T) {
	for _, doc := range []string{
		"review:\n  reviewers:\n    - agent: codex\n",
		"review:\n  fail_open: false\n",
	} {
		if _, err := LoadGlobalFromBytes([]byte(doc)); err == nil {
			t.Fatalf("LoadGlobalFromBytes(%q) succeeded, want the dropped panel key rejected", doc)
		}
	}
}

func TestReviewPolicyDefaults(t *testing.T) {
	got := Merge(DefaultGlobalConfig(), &RepoConfig{}).Review
	if got.MaxFixRounds != 3 {
		t.Fatalf("max_fix_rounds = %d, want 3", got.MaxFixRounds)
	}
	if got.FixRoundMinSeverity != "warning" {
		t.Fatalf("fix_round_min_severity = %q, want warning", got.FixRoundMinSeverity)
	}
}

func TestReviewMaxFixRoundsMergePrecedence(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("review:\n  max_fix_rounds: 5\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if got := Merge(global, &RepoConfig{}).Review.MaxFixRounds; got != 5 {
		t.Fatalf("inherited max_fix_rounds = %d, want 5", got)
	}
	repo, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: 0\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	if got := Merge(global, repo).Review.MaxFixRounds; got != 0 {
		t.Fatalf("repo override max_fix_rounds = %d, want 0 (uncapped)", got)
	}
}

func TestRepoReviewMaxFixRoundsIsTrustedOnly(t *testing.T) {
	one, two := 1, 2
	pushed := &RepoConfig{Review: ReviewRaw{MaxFixRounds: &one}}
	trusted := &RepoConfig{Review: ReviewRaw{MaxFixRounds: &two}}
	for _, allow := range []bool{false, true} {
		got := EffectiveRepoConfig(pushed, trusted, allow)
		if got.Review.MaxFixRounds == nil || *got.Review.MaxFixRounds != 2 {
			t.Fatalf("allow_repo_commands=%v: max_fix_rounds = %v, want trusted 2", allow, got.Review.MaxFixRounds)
		}
	}
}

func TestReviewMaxFixRoundsRejectsNegativeValues(t *testing.T) {
	for _, load := range []struct {
		name string
		fn   func() error
	}{
		{name: "global", fn: func() error {
			_, err := LoadGlobalFromBytes([]byte("review:\n  max_fix_rounds: -1\n"))
			return err
		}},
		{name: "repo", fn: func() error {
			_, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: -1\n"))
			return err
		}},
	} {
		t.Run(load.name, func(t *testing.T) {
			err := load.fn()
			if err == nil || !strings.Contains(err.Error(), "review.max_fix_rounds") {
				t.Fatalf("error = %v, want review.max_fix_rounds validation", err)
			}
		})
	}
}

func TestReviewFixRoundMinSeverityNormalizesAndValidates(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("review:\n  fix_round_min_severity: Error\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if got := Merge(global, &RepoConfig{}).Review.FixRoundMinSeverity; got != "error" {
		t.Fatalf("fix_round_min_severity = %q, want error", got)
	}
	_, err = LoadGlobalFromBytes([]byte("review:\n  fix_round_min_severity: critical\n"))
	if err == nil || !strings.Contains(err.Error(), "review.fix_round_min_severity") {
		t.Fatalf("error = %v, want review.fix_round_min_severity validation", err)
	}
}

func TestRetiredReviewMaxParallelRejectsNegative(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("review:\n  max_parallel: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "review.max_parallel") {
		t.Fatalf("error = %v, want review.max_parallel validation", err)
	}
}
