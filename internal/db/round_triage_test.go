package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewTriageRoundAccountingAndOverrideReason(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/triage-repo", "https://example.com/triage.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(step.ID, 2, "auto_fix", nil, nil, 1); err != nil {
		t.Fatal(err)
	}

	count, err := d.CountStepFixRounds(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("fix rounds = %d, want 1", count)
	}

	reason := "master triage: merge-blocking"
	if err := d.SetStepRoundFixOverrideReason(initial.ID, reason); err != nil {
		t.Fatal(err)
	}
	rounds, err := d.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rounds[0].FixOverrideReason == nil || *rounds[0].FixOverrideReason != reason {
		t.Fatalf("fix override reason = %v, want %q", rounds[0].FixOverrideReason, reason)
	}
}
