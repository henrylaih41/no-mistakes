package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AllInfoReviewCompletesWithZeroFixRounds(t *testing.T) {
	t.Run("demoted follow-ups complete in one round", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		findings := types.DemoteBelowSeverity(types.Findings{Items: []types.Finding{
			{ID: "review-1", Severity: types.FindingSeverityInfo, Action: types.ActionAutoFix, Description: "fix info"},
			{ID: "review-2", Severity: types.FindingSeverityInfo, Action: types.ActionAskMaster, Description: "ask about info"},
		}}, types.FindingSeverityWarning)
		findingsJSON, err := types.MarshalFindingsJSON(findings)
		if err != nil {
			t.Fatalf("marshal findings: %v", err)
		}

		callCount := 0
		step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{NeedsApproval: false, AutoFixable: true, Findings: findingsJSON}, nil
		}}
		cfg := &config.Config{
			AutoFix: config.AutoFix{Review: 3},
			Review:  config.Review{MaxFixRounds: 0, FixRoundMinSeverity: types.FindingSeverityWarning},
		}
		exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
		if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if callCount != 1 {
			t.Fatalf("step call count = %d, want 1", callCount)
		}

		storedRun, err := database.GetRun(run.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if storedRun.AwaitingAgentSince != nil {
			t.Fatalf("AwaitingAgentSince = %v, want nil", storedRun.AwaitingAgentSince)
		}
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatalf("get steps: %v", err)
		}
		if len(steps) != 1 || steps[0].Status != types.StepStatusCompleted {
			t.Fatalf("steps = %+v, want one completed review step", steps)
		}
		rounds, err := database.GetRoundsByStep(steps[0].ID)
		if err != nil {
			t.Fatalf("get rounds: %v", err)
		}
		if len(rounds) != 1 {
			t.Fatalf("round count = %d, want 1", len(rounds))
		}
		if steps[0].FindingsJSON == nil {
			t.Fatal("persisted step findings are nil")
		}
		persisted, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
		if err != nil {
			t.Fatalf("parse persisted findings: %v", err)
		}
		if len(persisted.Items) != 2 {
			t.Fatalf("persisted findings = %+v, want 2", persisted.Items)
		}
		for _, item := range persisted.Items {
			if item.Disposition != types.FindingDispositionFollowUp {
				t.Errorf("finding %s disposition = %q, want follow-up", item.ID, item.Disposition)
			}
		}
	})

	t.Run("undemoted auto-fix starts a fix round", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		findingsJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{{
			ID: "review-1", Severity: types.FindingSeverityInfo, Action: types.ActionAutoFix, Description: "fix info",
		}}})
		if err != nil {
			t.Fatalf("marshal findings: %v", err)
		}

		callCount := 0
		step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: false, AutoFixable: true, Findings: findingsJSON}, nil
			}
			return &StepOutcome{}, nil
		}}
		cfg := &config.Config{
			AutoFix: config.AutoFix{Review: 3},
			Review:  config.Review{MaxFixRounds: 0, FixRoundMinSeverity: types.FindingSeverityWarning},
		}
		exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
		if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if callCount != 2 {
			t.Fatalf("step call count = %d, want 2", callCount)
		}
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatalf("get steps: %v", err)
		}
		rounds, err := database.GetRoundsByStep(steps[0].ID)
		if err != nil {
			t.Fatalf("get rounds: %v", err)
		}
		if len(rounds) != 2 {
			t.Fatalf("round count = %d, want 2", len(rounds))
		}
	})
}
