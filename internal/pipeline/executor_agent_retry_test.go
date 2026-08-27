package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AgentTransientParksAndRetryResumesSameStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	calls := 0
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			return nil, &agent.TransientError{Agent: "claude", Label: "empty-stderr exit-1", Err: errors.New("claude exited: exit status 1:")}
		}
		return &StepOutcome{ExitCode: 0}, nil
	}}
	lint := newPassStep(types.StepLint)
	exec := NewExecutor(database, p, nil, nil, []Step{step, lint}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingRetry)
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("AwaitingAgentSince = nil while transient retry is parked")
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Error == nil || !strings.Contains(*steps[0].Error, "agent provider/transient failure") {
		t.Fatalf("parked error = %v, want transient failure reason", steps[0].Error)
	}
	if steps[1].Status != types.StepStatusPending {
		t.Fatalf("later step = %s, want pending", steps[1].Status)
	}

	if err := exec.Respond(types.StepTest, types.ActionRetry, nil); err != nil {
		t.Fatalf("retry response: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
	if calls != 2 || lint.callCount() != 1 {
		t.Fatalf("calls = step %d lint %d, want 2/1", calls, lint.callCount())
	}
}

func TestExecutor_AgentRetryDoesNotConsumeReviewFixRoundCap(t *testing.T) {
	database, p, run, repo := setupTest(t)
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			if _, err := sctx.DB.InsertStepRound(sctx.StepResultID, 1, "auto_fix", nil, nil, 1); err != nil {
				t.Fatal(err)
			}
			return nil, &agent.TransientError{Agent: "claude", Label: "http 503", Err: errors.New("503 service unavailable")}
		}
		return &StepOutcome{ExitCode: 0}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{Review: config.Review{MaxFixRounds: 1}}, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingRetry)
	if err := exec.Respond(types.StepReview, types.ActionRetry, nil); err != nil {
		t.Fatalf("retry at fix cap: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	steps, _ := database.GetStepsByRun(run.ID)
	count, err := database.CountStepFixRounds(steps[0].ID)
	if err != nil || count != 1 {
		t.Fatalf("fix round count = %d, %v; want 1", count, err)
	}
}

func TestExecutor_ResumeRestoresAgentRetryGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStep(stepResult.ID, types.StepStatusAwaitingRetry, "503 after retries", 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	step := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepTest, types.ActionRetry, nil); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("recovered retry gate never accepted response: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered retry gate timed out")
	}
	if step.callCount() != 1 {
		t.Fatalf("step calls = %d, want 1 after retry", step.callCount())
	}
	got, _ := database.GetRun(run.ID)
	if got.Status != types.RunCompleted || got.AwaitingAgentSince != nil {
		t.Fatalf("run = %s awaiting %v, want completed/unparked", got.Status, got.AwaitingAgentSince)
	}
}

func TestExecutor_AgentAutoRetryIsBoundedButManualRetryRemainsAvailable(t *testing.T) {
	database, p, run, repo := setupTest(t)
	calls := 0
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		if calls <= 2 {
			return nil, &agent.TransientError{Agent: "claude", Label: "http 503", Err: errors.New("503 service unavailable")}
		}
		return &StepOutcome{ExitCode: 0}, nil
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingRetry)
	if err := exec.RespondWithRetryAttribution(types.StepTest, types.ActionRetry, nil, nil, nil, "", true); err != nil {
		t.Fatalf("first auto retry: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.RespondWithRetryAttribution(types.StepTest, types.ActionRetry, nil, nil, nil, "", true)
		if err == nil {
			t.Fatal("second auto retry was accepted")
		}
		if strings.Contains(err.Error(), "auto-retry already consumed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second transient park not observed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := exec.Respond(types.StepTest, types.ActionRetry, nil); err != nil {
		t.Fatalf("manual retry after auto budget: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	steps, _ := database.GetStepsByRun(run.ID)
	auto, err := database.CountStepAgentAutoRetries(steps[0].ID)
	if err != nil || auto != 1 {
		t.Fatalf("auto retries = %d, %v; want 1", auto, err)
	}
}
