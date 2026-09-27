package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSetStepRoundFixOverride_RecordsTheReasonWithTheSelection(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, sr, round := seedRound(t, d, repo.ID, "feature", types.StepReview)

	if err := d.SetStepRoundFixOverride(round.ID, strPtr(`["keep-a"]`), nil, ""); err == nil {
		t.Fatal("an override without a reason was recorded")
	}
	if err := d.SetStepRoundFixOverride("missing-round", strPtr(`["keep-a"]`), nil, "why"); err == nil {
		t.Fatal("an override on a missing round reported success")
	}
	if err := d.SetStepRoundFixOverride(round.ID, strPtr(`["keep-a"]`), nil, "the residual is a real crash"); err != nil {
		t.Fatal(err)
	}

	rounds, err := d.GetRoundsByStep(sr.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds: %v %d", err, len(rounds))
	}
	got := rounds[0]
	if got.SelectionSource == nil || *got.SelectionSource != RoundSelectionSourceUserOverride {
		t.Fatalf("selection_source = %v, want %q", got.SelectionSource, RoundSelectionSourceUserOverride)
	}
	if got.FixOverrideReason == nil || *got.FixOverrideReason != "the residual is a real crash" {
		t.Fatalf("fix_override_reason = %v", got.FixOverrideReason)
	}
	if got.SelectedFindingIDs == nil || *got.SelectedFindingIDs != `["keep-a"]` {
		t.Fatalf("selected_finding_ids = %v", got.SelectedFindingIDs)
	}
}

// An override is a human fix decision, so it carries across runs on the
// branch like any other.
func TestGetBranchDecisionRounds_IncludesFixOverrides(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, _, round := seedRound(t, d, repo.ID, "feature", types.StepReview)
	if err := d.SetStepRoundFixOverride(round.ID, strPtr(`["keep-a"]`), nil, "why"); err != nil {
		t.Fatal(err)
	}
	current, err := d.InsertRun(repo.ID, "feature", "head2", "base")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := d.GetBranchDecisionRounds(repo.ID, "feature", current.ID, MaxBranchDecisionRounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Round.ID != round.ID {
		t.Fatalf("branch decisions = %+v, want the override round", got)
	}
}
