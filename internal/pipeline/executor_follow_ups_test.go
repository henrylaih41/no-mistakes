package pipeline

import (
	"context"
	"strings"
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

// TestExecutor_UnselectedFollowUpsDoNotVetoVerification checks that a
// follow-up the verification round reports cannot keep a fixed selected
// finding outstanding: one in the fixed finding's file used to trip the
// reported-file rule, and a fileless one used to refuse to clear anything.
func TestExecutor_UnselectedFollowUpsDoNotVetoVerification(t *testing.T) {
	for name, file := range map[string]string{"same file": "a.go", "no file": ""} {
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			followUps, err := types.MarshalFindingsJSON(types.DemoteBelowSeverity(types.Findings{Items: []types.Finding{
				{ID: "review-1", Severity: types.FindingSeverityInfo, Action: types.ActionAutoFix, File: file, Description: "rename helper"},
			}}, types.FindingSeverityWarning))
			if err != nil {
				t.Fatal(err)
			}
			reviewable := []string{"a.go"}
			round := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				round++
				if round == 1 {
					return &StepOutcome{
						NeedsApproval:   true,
						Findings:        `{"findings":[{"id":"review-1","severity":"warning","file":"a.go","line":3,"description":"missing error check","action":"ask-user"}],"summary":"1 finding"}`,
						ReviewedPaths:   reviewable,
						ReviewablePaths: reviewable,
					}, nil
				}
				return &StepOutcome{NeedsApproval: true, Findings: followUps, ReviewedPaths: reviewable, ReviewablePaths: reviewable}, nil
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			done, _ := startExecutor(t, exec, run, repo, t.TempDir())

			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
				t.Fatal(err)
			}
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if steps[0].FindingsJSON == nil {
				t.Fatal("the gate lost the round's follow-up")
			}
			if strings.Contains(*steps[0].FindingsJSON, "missing error check") {
				t.Fatalf("the verified finding stayed outstanding because of a follow-up: %s", *steps[0].FindingsJSON)
			}
			if !strings.Contains(*steps[0].FindingsJSON, "rename helper") {
				t.Fatalf("the round's follow-up is missing from the gate: %s", *steps[0].FindingsJSON)
			}

			if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			waitExecutorDone(t, done)
		})
	}
}
