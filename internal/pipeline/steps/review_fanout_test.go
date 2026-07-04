package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

func TestReviewStep_NoPanelSingleReviewerKeepsLegacyFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	impl := &mockAgent{name: "impl", runFn: reviewReturning(Findings{
		Items: []Finding{{ID: "agent-id", Severity: "warning", Description: "legacy issue", Action: "auto-fix"}},
	})}

	sctx := newTestContext(t, impl, dir, baseSHA, headSHA, config.Commands{})

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(merged.Items), merged.Items)
	}
	if merged.Items[0].ID != "agent-id" {
		t.Errorf("legacy finding id = %q, want agent-id", merged.Items[0].ID)
	}
	if merged.Items[0].Source != "" {
		t.Errorf("legacy finding source = %q, want unstamped", merged.Items[0].Source)
	}
	if len(impl.calls) != 1 {
		t.Fatalf("expected impl agent to run once, got %d", len(impl.calls))
	}
	if impl.calls[0].OnChunk == nil {
		t.Error("expected legacy single-reviewer path to stream chunks")
	}
}

func TestReviewStep_FanOut_SingleConfiguredReviewerUsesPanelAttribution(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{ID: "model-id", Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
	})}
	fixAgent := &mockAgent{name: "fixer", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("fix agent must not run during the review pass")
		return nil, nil
	}}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex}
	sctx.ReviewPanel = true

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(merged.Items), merged.Items)
	}
	if merged.Items[0].ID != "review-codex-1-1" {
		t.Errorf("single-panel finding id = %q, want review-codex-1-1", merged.Items[0].ID)
	}
	if merged.Items[0].Source != "codex" {
		t.Errorf("single-panel finding source = %q, want codex", merged.Items[0].Source)
	}
	if len(codex.calls) != 1 {
		t.Fatalf("expected codex to run once, got %d", len(codex.calls))
	}
	if codex.calls[0].OnChunk == nil {
		t.Error("expected single-reviewer panel to keep live streaming")
	}
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
	sctx.ReviewPanel = true

	step := &ReviewStep{}
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
	sctx.ReviewPanel = true
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","description":"earlier","action":"auto-fix"}],"summary":"1 issue"}`

	step := &ReviewStep{}
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
	sctx.ReviewPanel = true
	// Config.Review.FailOpen defaults to false (fail-closed).

	step := &ReviewStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected the step to fail closed when a reviewer errors")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the failed reviewer family, got %q", err)
	}
}

func TestReviewStep_FanOut_FailClosedCancelsSiblingAndNamesOriginalFailure(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	started := make(chan struct{})
	codexCanceled := make(chan struct{})
	codex := &mockAgent{name: "codex", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		close(started)
		<-ctx.Done()
		close(codexCanceled)
		return nil, ctx.Err()
	}}
	claude := &mockAgent{name: "claude", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		select {
		case <-started:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return nil, errors.New("codex reviewer did not start")
		}
		return nil, errors.New("backend exploded")
	}}
	fixAgent := &mockAgent{name: "fixer"}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	sctx.Reviewers = []agent.Agent{codex, claude}
	sctx.ReviewPanel = true

	step := &ReviewStep{}
	done := make(chan error, 1)
	go func() {
		_, err := step.Execute(sctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected fail-closed panel to fail")
		}
		if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "backend exploded") {
			t.Fatalf("error should name the genuinely failing reviewer, got %q", err)
		}
	case <-time.After(500 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("fail-closed panel did not return promptly after first reviewer error")
	}

	select {
	case <-codexCanceled:
	default:
		t.Fatal("expected sibling reviewer to be cancelled")
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
	sctx.ReviewPanel = true
	sctx.Config.Review.FailOpen = true

	step := &ReviewStep{}
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

func TestReviewStep_FanOut_FailOpenWaitsForSiblingCompletion(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	started := make(chan struct{})
	release := make(chan struct{})
	codexCompleted := make(chan struct{})
	codex := &mockAgent{name: "codex", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			close(codexCompleted)
			out, _ := json.Marshal(Findings{
				Items: []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
			})
			return &agent.Result{Output: out}, nil
		}
	}}
	claude := &mockAgent{name: "claude", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		<-started
		return nil, errors.New("backend exploded")
	}}
	fixAgent := &mockAgent{name: "fixer"}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}
	sctx.ReviewPanel = true
	sctx.Config.Review.FailOpen = true

	step := &ReviewStep{}
	type stepResult struct {
		outcome *pipeline.StepOutcome
		err     error
	}
	done := make(chan stepResult, 1)
	go func() {
		outcome, err := step.Execute(sctx)
		done <- stepResult{outcome: outcome, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("codex reviewer did not start")
	}
	select {
	case got := <-done:
		t.Fatalf("fail-open panel returned before surviving reviewer completed: outcome=%v err=%v", got.outcome, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	var got stepResult
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fail-open panel did not return after surviving reviewer completed")
	}
	if got.err != nil {
		t.Fatalf("fail-open should survive one reviewer error: %v", got.err)
	}
	select {
	case <-codexCompleted:
	default:
		t.Fatal("expected surviving reviewer to complete normally")
	}
	merged, err := types.ParseFindingsJSON(got.outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 1 || merged.Items[0].Source != "codex" {
		t.Fatalf("expected codex finding to survive, got %+v", merged.Items)
	}
}
