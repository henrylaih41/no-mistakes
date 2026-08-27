package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewReturning builds a mockAgent runFn that returns the given findings as
// the agent's structured review output.
func reviewReturning(f Findings) func(context.Context, agent.RunOpts) (*agent.Result, error) {
	return func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		j, _ := json.Marshal(f)
		return &agent.Result{Output: j}, nil
	}
}

func findingBySource(items []Finding, source string) (Finding, bool) {
	for _, item := range items {
		if item.Source == source {
			return item, true
		}
	}
	return Finding{}, false
}

func TestReviewStep_FanOut_InitialReviewMergesBothReviewers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items:         []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
		RiskLevel:     "medium",
		RiskRationale: "codex rationale",
		Summary:       "codex summary",
	})}
	claude := &mockAgent{name: "claude", runFn: reviewReturning(Findings{
		Items:         []Finding{{Severity: "error", Description: "claude issue", Action: "ask-user"}},
		RiskLevel:     "high",
		RiskRationale: "claude rationale",
		Summary:       "claude summary",
	})}
	// The fix/implementation agent must never run during an initial review.
	fixAgent := &mockAgent{name: "fixer", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("fix agent must not run during the review pass")
		return nil, nil
	}}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}

	step := newTestReviewStep()
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 2 {
		t.Fatalf("expected 2 merged findings, got %d: %+v", len(merged.Items), merged.Items)
	}

	codexFinding, ok := findingBySource(merged.Items, "codex")
	if !ok {
		t.Fatal("expected a finding sourced from codex")
	}
	if codexFinding.ID != "review-codex-1-1" {
		t.Errorf("codex finding id = %q, want review-codex-1-1", codexFinding.ID)
	}
	claudeFinding, ok := findingBySource(merged.Items, "claude")
	if !ok {
		t.Fatal("expected a finding sourced from claude")
	}
	if claudeFinding.ID != "review-claude-2-1" {
		t.Errorf("claude finding id = %q, want review-claude-2-1", claudeFinding.ID)
	}

	// RiskLevel is the max across reviewers; an error finding needs approval.
	if merged.RiskLevel != "high" {
		t.Errorf("merged RiskLevel = %q, want high", merged.RiskLevel)
	}
	if !outcome.NeedsApproval {
		t.Error("expected NeedsApproval when a reviewer reports an error finding")
	}

	// Each reviewer ran exactly once, with streaming disabled in panel mode.
	if len(codex.calls) != 1 || len(claude.calls) != 1 {
		t.Fatalf("expected each reviewer to run once, got codex=%d claude=%d", len(codex.calls), len(claude.calls))
	}
	if codex.calls[0].OnChunk != nil || claude.calls[0].OnChunk != nil {
		t.Error("expected OnChunk to be nil in panel mode (not goroutine-safe)")
	}
}

func TestReviewStep_FanOut_RunsInFixMode(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	fixAgent := &mockAgent{name: "fixer", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		os.WriteFile(filepath.Join(dir, "fanout-fix.txt"), []byte("fixed"), 0o644)
		return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}, nil
	}}
	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
	})}
	claude := &mockAgent{name: "claude", runFn: reviewReturning(Findings{
		Items: []Finding{{Severity: "info", Description: "claude note", Action: "no-op"}},
	})}

	sctx := newTestContextWithDBRecords(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","description":"earlier","action":"auto-fix"}],"summary":"1 issue"}`

	step := newTestReviewStep()
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// The single fix agent ran once; the full panel re-reviewed the fixed code.
	if len(fixAgent.calls) != 1 {
		t.Errorf("expected fix agent to run once, got %d", len(fixAgent.calls))
	}
	if len(codex.calls) != 1 || len(claude.calls) != 1 {
		t.Fatalf("expected each reviewer to re-review once in fix mode, got codex=%d claude=%d", len(codex.calls), len(claude.calls))
	}
	if outcome.FixSummary != "address findings" {
		t.Errorf("FixSummary = %q, want 'address findings'", outcome.FixSummary)
	}

	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findingBySource(merged.Items, "codex"); !ok {
		t.Error("expected a codex-sourced finding after fix-mode re-review")
	}
	if _, ok := findingBySource(merged.Items, "claude"); !ok {
		t.Error("expected a claude-sourced finding after fix-mode re-review")
	}
}

func TestReviewStep_FanOut_FailClosedFailsStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
	})}
	claude := &mockAgent{name: "claude", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("reviewer crashed")
	}}
	fixAgent := &mockAgent{name: "fixer"}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}
	// Config.Review.FailOpen defaults to false (fail-closed).

	step := newTestReviewStep()
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected the step to fail closed when a reviewer errors")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the failed reviewer family, got %q", err)
	}
}

func TestReviewStep_FanOut_FailOpenContinues(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items:     []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
		RiskLevel: "medium",
	})}
	claude := &mockAgent{name: "claude", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("reviewer crashed")
	}}
	fixAgent := &mockAgent{name: "fixer"}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}
	sctx.Config.Review.FailOpen = true

	step := newTestReviewStep()
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("fail-open should survive a single reviewer error: %v", err)
	}
	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected only the surviving reviewer's finding, got %d", len(merged.Items))
	}
	if merged.Items[0].Source != "codex" {
		t.Errorf("surviving finding source = %q, want codex", merged.Items[0].Source)
	}
}

func TestReviewStep_FanOut_FailOpenCannotDropInvalidVerdict(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	valid := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items:         []Finding{{Severity: "warning", Description: "valid codex defect", Action: types.ActionAutoFix}},
		RiskLevel:     "low",
		RiskRationale: "clean",
	})}
	invalid := &mockAgent{
		name:                   "claude",
		preserveReviewEvidence: true,
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{
				Output:  []byte(`{"findings":[],"summary":"clean","risk_level":"low"}`),
				Metrics: &agent.InvocationMetrics{ModelRoundtrips: 1},
			}, nil
		},
	}
	sctx := newTestContext(t, &mockAgent{name: "fixer"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{valid, invalid}
	sctx.Config.Review.FailOpen = true
	sctx.Config.Review.Reviewers = []config.ReviewerSpec{{Agent: types.AgentCodex}, {Agent: types.AgentClaude}}

	outcome, err := newTestReviewStep().Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsTriage {
		t.Fatal("invalid verdict must park even when review.fail_open=true")
	}
	if got := len(invalid.calls); got != 2 {
		t.Fatalf("invalid reviewer calls = %d, want initial + one retry", got)
	}
	if got := len(valid.calls); got != 1 {
		t.Fatalf("valid reviewer calls = %d, want 1", got)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse triage findings: %v", err)
	}
	if !types.HasReviewVerdictEvidenceFinding(findings) {
		t.Fatalf("triage findings = %+v, want evidence finding", findings.Items)
	}
	preserved, ok := findingBySource(findings.Items, "codex")
	if !ok || preserved.Description != "valid codex defect" {
		t.Fatalf("triage findings = %+v, want preserved codex report", findings.Items)
	}
}

func TestReviewStep_FanOut_InvalidVerdictRetryErrorPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failOpen bool
	}{
		{name: "fail closed", failOpen: false},
		{name: "fail open", failOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			valid := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
				Items: []Finding{{Severity: "warning", Description: "valid codex defect", Action: types.ActionAutoFix}},
			})}
			call := 0
			invalid := &mockAgent{
				name:                   "claude",
				preserveReviewEvidence: true,
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					call++
					if call == 2 {
						return nil, errors.New("cold retry crashed")
					}
					return &agent.Result{
						Output:  []byte(`{"findings":[],"summary":"clean"}`),
						Metrics: &agent.InvocationMetrics{ModelRoundtrips: 1},
					}, nil
				},
			}
			sctx := newTestContext(t, &mockAgent{name: "fixer"}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Reviewers = []agent.Agent{valid, invalid}
			sctx.Config.Review.FailOpen = tc.failOpen
			var logs, audit []string
			sctx.Log = func(line string) { logs = append(logs, line) }
			sctx.LogFile = func(line string) { audit = append(audit, line) }

			outcome, err := newTestReviewStep().Execute(sctx)
			if !tc.failOpen {
				if err == nil || !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "cold retry crashed") {
					t.Fatalf("Execute() error = %v, want retry process failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if !outcome.NeedsTriage {
				t.Fatal("invalid verdict followed by retry error must park for triage")
			}
			if !sliceContainsText(logs, "claude", "DROPPED") || !sliceContainsText(audit, "claude", "cold retry crashed") {
				t.Fatalf("retry process error was not surfaced; logs=%v audit=%v", logs, audit)
			}
			findings, parseErr := types.ParseFindingsJSON(outcome.Findings)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if !types.HasReviewVerdictEvidenceFinding(findings) {
				t.Fatalf("triage findings = %+v, want evidence finding", findings.Items)
			}
			if !strings.Contains(findings.Items[0].Description, "insufficient activity") || !strings.Contains(findings.Items[0].Description, "cold retry crashed") {
				t.Fatalf("evidence reason = %q, want initial failure and retry error", findings.Items[0].Description)
			}
			if preserved, ok := findingBySource(findings.Items, "codex"); !ok || preserved.Description != "valid codex defect" {
				t.Fatalf("triage findings = %+v, want preserved valid report", findings.Items)
			}
		})
	}
}

func TestReviewStep_FanOut_InvalidVerdictRetryCanRecover(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	call := 0
	recovering := &mockAgent{
		name:                   "grok",
		preserveReviewEvidence: true,
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			call++
			rounds := 1
			if call == 2 {
				rounds = 2
			}
			return &agent.Result{
				Output:  []byte(`{"findings":[],"summary":"clean","risk_level":"low"}`),
				Metrics: &agent.InvocationMetrics{ModelRoundtrips: rounds},
			}, nil
		},
	}
	sctx := newTestContext(t, &mockAgent{name: "fixer"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{recovering}
	sctx.Config.Review.Reviewers = []config.ReviewerSpec{{Agent: types.AgentGrok}}

	outcome, err := newTestReviewStep().Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.NeedsTriage {
		t.Fatal("valid retry must clear evidence triage")
	}
	if len(recovering.calls) != 2 {
		t.Fatalf("reviewer calls = %d, want initial plus one cold retry", len(recovering.calls))
	}
}

func TestReviewStep_FanOut_ProcessErrorWinsBeforeEvidenceTriage(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	valid := &mockAgent{name: "codex", runFn: reviewReturning(Findings{Summary: "valid"})}
	invalid := &mockAgent{
		name:                   "claude",
		preserveReviewEvidence: true,
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean"}`), Metrics: &agent.InvocationMetrics{ModelRoundtrips: 1}}, nil
		},
	}
	failed := &mockAgent{name: "grok", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("reviewer crashed")
	}}
	sctx := newTestContext(t, &mockAgent{name: "fixer"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{valid, invalid, failed}

	_, err := newTestReviewStep().Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "grok") {
		t.Fatalf("Execute() error = %v, want fail-closed process error", err)
	}
	if len(invalid.calls) != 2 {
		t.Fatalf("invalid reviewer calls = %d, want initial plus cold retry", len(invalid.calls))
	}
}

func TestReviewStep_FanOut_FailOpenReportsProcessErrorBeforeEvidenceTriage(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	valid := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{Severity: "warning", Description: "valid source defect", Action: types.ActionAutoFix}},
	})}
	invalid := &mockAgent{
		name:                   "claude",
		preserveReviewEvidence: true,
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean"}`), Metrics: &agent.InvocationMetrics{ModelRoundtrips: 1}}, nil
		},
	}
	failed := &mockAgent{name: "grok", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("reviewer crashed")
	}}
	sctx := newTestContext(t, &mockAgent{name: "fixer"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{valid, invalid, failed}
	sctx.Config.Review.FailOpen = true
	var logs, audit []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	sctx.LogFile = func(line string) { audit = append(audit, line) }

	outcome, err := newTestReviewStep().Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsTriage {
		t.Fatal("remaining invalid verdict must park for triage")
	}
	if !sliceContainsText(logs, "grok", "DROPPED") || !sliceContainsText(audit, "grok", "ERROR") {
		t.Fatalf("process error was not surfaced; logs=%v audit=%v", logs, audit)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findingBySource(findings.Items, "codex"); !ok {
		t.Fatalf("valid reviewer finding was not preserved: %+v", findings.Items)
	}
}

func sliceContainsText(lines []string, parts ...string) bool {
	for _, line := range lines {
		matched := true
		for _, part := range parts {
			matched = matched && strings.Contains(line, part)
		}
		if matched {
			return true
		}
	}
	return false
}
