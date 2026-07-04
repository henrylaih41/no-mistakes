package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const reviewCapFinding = `{"findings":[{"id":"review-1","severity":"error","description":"still wrong","action":"auto-fix"}],"summary":"1 issue"}`

func TestExecutor_ReviewMaxFixRoundsParksAtTriageAndRejectsPlainFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      reviewCapFinding,
			}, nil
		},
	}

	cfg := &config.Config{Review: config.Review{MaxFixRounds: 1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatalf("first fix should be allowed: %v", err)
	}

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingTriage)
	if callCount != 2 {
		t.Fatalf("call count = %d, want 2 (initial + one fix round)", callCount)
	}

	err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"})
	if err == nil {
		t.Fatal("plain fix at cap succeeded, want structured refusal")
	}
	if !strings.Contains(err.Error(), "max_fix_rounds") || !strings.Contains(err.Error(), "1") {
		t.Fatalf("error = %q, want cap detail", err.Error())
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if steps[0].Status != types.StepStatusAwaitingTriage {
		t.Fatalf("status after rejected fix = %s, want %s", steps[0].Status, types.StepStatusAwaitingTriage)
	}
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("AwaitingAgentSince cleared by rejected fix, want gate to remain parked")
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve after rejected fix: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_ReviewMaxFixRoundsOverrideRequiresReasonAndPersistsIt(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount < 3 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      reviewCapFinding,
				}, nil
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

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatalf("first fix should be allowed: %v", err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingTriage)

	if err := exec.RespondWithOverrideReason(types.StepReview, types.ActionFix, []string{"review-1"}, nil, nil, "   "); err == nil {
		t.Fatal("empty override reason succeeded, want rejection")
	}

	reason := "master triage: residual finding is merge-blocking"
	if err := exec.RespondWithOverrideReason(types.StepReview, types.ActionFix, []string{"review-1"}, nil, nil, reason); err != nil {
		t.Fatalf("override fix with reason: %v", err)
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
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) < 2 {
		t.Fatalf("rounds = %d, want at least 2", len(rounds))
	}
	triggerRound := rounds[1]
	if triggerRound.SelectionSource == nil || *triggerRound.SelectionSource != db.RoundSelectionSourceUserOverride {
		t.Fatalf("selection_source = %v, want %q", triggerRound.SelectionSource, db.RoundSelectionSourceUserOverride)
	}
	if triggerRound.FixOverrideReason == nil || *triggerRound.FixOverrideReason != reason {
		t.Fatalf("fix_override_reason = %v, want %q", triggerRound.FixOverrideReason, reason)
	}
}

func TestExecutor_ReviewMaxFixRoundsZeroPreservesUnboundedFixes(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount < 3 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      reviewCapFinding,
				}, nil
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	cfg := &config.Config{Review: config.Review{MaxFixRounds: 0}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatalf("first fix: %v", err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatalf("second fix with cap disabled: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_ReviewMaxFixRoundsCountsAutoFixRounds(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      reviewCapFinding,
			}, nil
		},
	}

	cfg := &config.Config{
		AutoFix: config.AutoFix{Review: 3},
		Review:  config.Review{MaxFixRounds: 1},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingTriage)
	if callCount != 2 {
		t.Fatalf("call count = %d, want 2 (initial + one capped auto-fix)", callCount)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve at triage: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_ReviewMaxFixRoundsNegativeFailsLoud(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      reviewCapFinding,
			}, nil
		},
	}

	cfg := &config.Config{Review: config.Review{MaxFixRounds: -1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected negative max_fix_rounds to fail loudly")
	}
	if !strings.Contains(err.Error(), "review.max_fix_rounds") {
		t.Fatalf("error = %v, want review.max_fix_rounds", err)
	}
}
