package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ApprovalFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Step that needs approval on first call, passes on second
	callCount := 0
	var step Step = &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"issues":["bug"]}`}, nil
			}
			// After fix, re-evaluate passes
			return &StepOutcome{NeedsApproval: false, ExitCode: 0}, nil
		},
	}

	steps := []Step{step, newPassStep(types.StepTest)}
	exec := NewExecutor(database, p, nil, nil, steps, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Wait for awaiting_approval
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Send fix action
	exec.Respond(types.StepReview, types.ActionFix, nil)

	// Wait for step to re-execute and complete (it passes on second call)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	// Both steps should be completed
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].Status != types.StepStatusCompleted {
		t.Errorf("review: expected %q, got %q", types.StepStatusCompleted, dbSteps[0].Status)
	}
	if dbSteps[1].Status != types.StepStatusCompleted {
		t.Errorf("test: expected %q, got %q", types.StepStatusCompleted, dbSteps[1].Status)
	}

	// Step should have been called twice (initial + after fix)
	if callCount != 2 {
		t.Errorf("expected step to be called 2 times, got %d", callCount)
	}
}

func TestExecutor_AwaitingAgentMarkerSetOnGateClearedOnRespond(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Entering the gate flips the pollable parked marker on.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run while parked: %v", err)
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("AwaitingAgentSince = nil while parked at gate, want a timestamp")
	}

	// Responding clears it as the run resumes, so the marker is non-nil only
	// while the run is actually parked awaiting the agent.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("respond: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	resumed, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run after respond: %v", err)
	}
	if resumed.AwaitingAgentSince != nil {
		t.Errorf("AwaitingAgentSince = %d after respond, want nil", *resumed.AwaitingAgentSince)
	}
}

func TestExecutor_CancelledApprovalGateFailsAtomically(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, `{"findings":[{"severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, t.TempDir())
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("executor error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not leave cancelled approval gate")
	}

	failedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedRun.Status != types.RunFailed || failedRun.AwaitingAgentSince != nil {
		t.Fatalf("run = status %s awaiting %v, want failed and unparked", failedRun.Status, failedRun.AwaitingAgentSince)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusFailed {
		t.Fatalf("steps = %+v, want one failed step", steps)
	}
}

func TestExecutor_AgentTransientParksAndRetryResumesSameStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			if calls == 1 {
				return nil, &agent.TransientError{
					Agent: "claude",
					Label: "empty-stderr exit-1",
					Err:   errors.New("claude exited: exit status 1:"),
				}
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}
	lint := newPassStep(types.StepLint)
	exec := NewExecutor(database, p, nil, nil, []Step{step, lint}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingRetry)
	parkedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get parked run: %v", err)
	}
	if parkedRun.Status != types.RunRunning {
		t.Fatalf("run status = %s, want %s", parkedRun.Status, types.RunRunning)
	}
	if parkedRun.AwaitingAgentSince == nil {
		t.Fatal("AwaitingAgentSince = nil while agent retry is parked")
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if steps[0].Error == nil || !strings.Contains(*steps[0].Error, "agent provider/transient failure: claude empty-stderr exit-1") {
		t.Fatalf("parked step error = %v, want transient reason", steps[0].Error)
	}
	if steps[1].Status != types.StepStatusPending {
		t.Fatalf("later step status = %s, want pending before retry", steps[1].Status)
	}

	if err := exec.Respond(types.StepTest, types.ActionRetry, nil); err != nil {
		t.Fatalf("retry response: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
	if calls != 2 {
		t.Fatalf("step calls = %d, want 2", calls)
	}
	if lint.callCount() != 1 {
		t.Fatalf("later step calls = %d, want 1", lint.callCount())
	}
}

func TestExecutor_AgentRetryDoesNotCollideWithReviewFixRoundCap(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			if calls == 1 {
				if _, err := sctx.DB.InsertStepRound(sctx.StepResultID, 1, "auto_fix", nil, nil, 1); err != nil {
					t.Fatalf("seed consumed review fix round: %v", err)
				}
				return nil, &agent.TransientError{
					Agent: "claude",
					Label: "empty-stderr exit-1",
					Err:   errors.New("claude exited: exit status 1:"),
				}
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}
	cfg := &config.Config{Review: config.Review{MaxFixRounds: 1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingRetry)
	if err := exec.Respond(types.StepReview, types.ActionRetry, nil); err != nil {
		t.Fatalf("retry response should not require fix override at cap: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	count, err := database.CountStepFixRounds(steps[0].ID)
	if err != nil {
		t.Fatalf("count fix rounds: %v", err)
	}
	if count != 1 {
		t.Fatalf("fix round count = %d, want original seeded count 1", count)
	}
}

func TestExecutor_AutoApprovesParkedGateWhenResolverPasses(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	resolverCalls := make(chan struct{}, 1)
	step := &adaptiveCallStep{
		name: types.StepCI,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"warning","description":"PR was still open when CI monitoring timed out","action":"ask-user"}],"summary":"CI monitoring timed out before PR was merged or closed"}`,
				ApprovalAutoResolve: func(context.Context) bool {
					select {
					case resolverCalls <- struct{}{}:
					default:
					}
					return true
				},
				ApprovalAutoResolveInterval: time.Millisecond,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step, newPassStep(types.StepTest)}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not auto-approve parked gate")
	}

	select {
	case <-resolverCalls:
	default:
		t.Fatal("expected approval auto-resolver to run")
	}

	dbRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if dbRun.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want %s", dbRun.Status, types.RunCompleted)
	}
	if dbRun.AwaitingAgentSince != nil {
		t.Fatalf("AwaitingAgentSince = %d after auto-approval, want nil", *dbRun.AwaitingAgentSince)
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if dbSteps[0].Status != types.StepStatusCompleted {
		t.Fatalf("ci step status = %s, want %s", dbSteps[0].Status, types.StepStatusCompleted)
	}
	if dbSteps[1].Status != types.StepStatusCompleted {
		t.Fatalf("test step status = %s, want %s", dbSteps[1].Status, types.StepStatusCompleted)
	}
}

func TestExecutor_ResumeRestoresParkedGateAndReviewSessions(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs a fix","action":"ask-user"}],"summary":"one issue"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 25); err != nil {
		t.Fatal(err)
	}
	if _, err := database.EnterApprovalGate(context.Background(), run.ID, stepResult.ID, types.StepStatusAwaitingApproval, 25, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleReviewer), "fake", "reviewer-session"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), "fake", "fixer-session"); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	fake := newFakeSessionAgent()
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if !sctx.Fixing {
				return nil, fmt.Errorf("recovered gate must not rerun its completed review pass")
			}
			if _, err := sctx.RunAgentSession(SessionRoleFixer, agent.RunOpts{Prompt: "fix"}); err != nil {
				return nil, err
			}
			if _, err := sctx.RunAgentSession(SessionRoleReviewer, agent.RunOpts{Prompt: "rereview"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{SessionReuse: true}, fake, []Step{step}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Resume(context.Background(), run, repo, t.TempDir())
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered gate never accepted a response: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered executor timed out")
	}

	if len(fake.calls) != 2 {
		t.Fatalf("agent invocations = %d, want fixer and rereviewer", len(fake.calls))
	}
	if fake.calls[0].session == nil || fake.calls[0].session.ID != "fixer-session" {
		t.Fatalf("fixer session = %+v, want fixer-session", fake.calls[0].session)
	}
	if fake.calls[1].session == nil || fake.calls[1].session.ID != "reviewer-session" {
		t.Fatalf("reviewer session = %+v, want reviewer-session", fake.calls[1].session)
	}
	resumed, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != types.RunCompleted || resumed.AwaitingAgentSince != nil {
		t.Fatalf("recovered run = status %s awaiting %v, want completed and unparked", resumed.Status, resumed.AwaitingAgentSince)
	}
}

func TestExecutor_TracksApprovalAndUserFixTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"severity":"error","description":"bug one","action":"auto-fix"},{"severity":"warn","description":"bug two","action":"ask-user"}],"summary":"2 issues"}`}, nil
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentClaude}, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatalf("respond error: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	approvalEvent := recorder.find("approval", "action", "fix")
	if approvalEvent == nil {
		t.Fatal("expected approval telemetry event")
	}
	if got := approvalEvent.fields["step"]; got != string(types.StepReview) {
		t.Fatalf("approval step = %v, want %q", got, types.StepReview)
	}
	if got := approvalEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("approval selected_findings_count = %v, want 2", got)
	}

	fixEvent := recorder.find("fix", "source", "user")
	if fixEvent == nil {
		t.Fatal("expected user fix telemetry event")
	}
	if got := fixEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("fix selected_findings_count = %v, want 2", got)
	}

	stepEvent := recorder.find("step", "status", string(types.StepStatusAwaitingApproval))
	if stepEvent == nil {
		t.Fatal("expected awaiting approval step telemetry event")
	}
	if got := stepEvent.fields["findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("step findings_count = %v, want 2", got)
	}
	if got := stepEvent.fields["agent"]; got != string(types.AgentClaude) {
		t.Fatalf("step agent = %v, want %q", got, types.AgentClaude)
	}
}

func TestExecutor_TracksAutoFixTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					AutoFixable: true,
					Findings:    `{"findings":[{"severity":"error","description":"fix me","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	cfg := &config.Config{Agent: types.AgentClaude, AutoFix: config.AutoFix{Review: 1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	fixEvent := recorder.find("fix", "source", "auto")
	if fixEvent == nil {
		t.Fatal("expected auto-fix telemetry event")
	}
	if got := fixEvent.fields["step"]; got != string(types.StepReview) {
		t.Fatalf("fix step = %v, want %q", got, types.StepReview)
	}
	if got := fixEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "1" {
		t.Fatalf("fix selected_findings_count = %v, want 1", got)
	}
	if got := fixEvent.fields["attempt"]; fmt.Sprint(got) != "1" {
		t.Fatalf("fix attempt = %v, want 1", got)
	}
}
