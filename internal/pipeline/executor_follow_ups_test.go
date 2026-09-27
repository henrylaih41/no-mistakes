package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AllFollowUpReviewCompletesWithoutAFixRoundOrPark(t *testing.T) {
	database, p, run, repo := setupTest(t)
	findings := types.DemoteBelowSeverity(types.Findings{Items: []types.Finding{
		{ID: "review-1", Severity: types.FindingSeverityInfo, Action: types.ActionAutoFix, Description: "fix info"},
		{ID: "review-2", Severity: types.FindingSeverityInfo, Action: types.ActionAskUser, Description: "ask about info"},
	}}, types.FindingSeverityWarning)
	findingsJSON, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		return &StepOutcome{AutoFixable: true, Findings: findingsJSON}, nil
	}}
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 1 {
		t.Fatalf("review ran %d times, want 1 (no fix round for follow-ups)", calls)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("steps = %+v, want one completed review step", steps)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AwaitingAgentSince != nil {
		t.Fatal("follow-ups parked the run")
	}
}

// TestExecutor_FollowUpsNeverJoinTheOutstandingSet checks that a follow-up an
// earlier round reported is not carried into a later round's findings: the
// round that raised it reported it, and nothing will ever verify it.
func TestExecutor_FollowUpsNeverJoinTheOutstandingSet(t *testing.T) {
	database, p, run, repo := setupTest(t)
	first := types.Findings{Items: []types.Finding{
		{ID: "review-1", Severity: types.FindingSeverityWarning, Action: types.ActionAutoFix, File: "a.go", Description: "missing error check"},
		{ID: "review-2", Severity: types.FindingSeverityInfo, Action: types.ActionAutoFix, File: "b.go", Description: "rename helper"},
	}}
	first = types.DemoteBelowSeverity(first, types.FindingSeverityWarning)
	firstJSON, err := types.MarshalFindingsJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	reviewable := []string{"a.go", "b.go"}
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			return &StepOutcome{AutoFixable: true, NeedsApproval: true, Findings: firstJSON, ReviewedPaths: reviewable, ReviewablePaths: reviewable}, nil
		}
		return &StepOutcome{ReviewedPaths: reviewable, ReviewablePaths: reviewable}, nil
	}}
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("review ran %d times, want an initial round and one verification round", calls)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("rounds = %d, want 2", len(rounds))
	}
	if rounds[1].FindingsJSON != nil {
		carried, err := types.ParseFindingsJSON(*rounds[1].FindingsJSON)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range carried.Items {
			if f.ID == "review-2" {
				t.Fatalf("round 2 carried the round-1 follow-up forward: %s", *rounds[1].FindingsJSON)
			}
		}
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("review status = %s, want completed", steps[0].Status)
	}
}
