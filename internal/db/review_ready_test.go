package db

import "testing"

func TestMarkRunReviewReadyStampsOnceAndHeadChangeClears(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "head1", "base1")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if run.ReviewReadySince != nil {
		t.Fatalf("new run ReviewReadySince = %v, want nil", *run.ReviewReadySince)
	}

	// First stamp records a ready timestamp.
	before := now()
	if err := d.MarkRunReviewReady(run.ID); err != nil {
		t.Fatalf("mark review ready: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.ReviewReadySince == nil {
		t.Fatal("ReviewReadySince = nil after MarkRunReviewReady, want a timestamp")
	}
	if *got.ReviewReadySince < before {
		t.Errorf("ReviewReadySince = %d, want >= %d", *got.ReviewReadySince, before)
	}
	stamped := *got.ReviewReadySince

	// Idempotent: a second stamp preserves the first ready moment.
	if err := d.MarkRunReviewReady(run.ID); err != nil {
		t.Fatalf("second mark review ready: %v", err)
	}
	got, _ = d.GetRun(run.ID)
	if got.ReviewReadySince == nil || *got.ReviewReadySince != stamped {
		t.Fatalf("ReviewReadySince = %v after second stamp, want unchanged %d", got.ReviewReadySince, stamped)
	}

	// A head advance rearms the marker (clears it back to nil).
	if err := d.UpdateRunHeadSHA(run.ID, "head2"); err != nil {
		t.Fatalf("update head sha: %v", err)
	}
	got, _ = d.GetRun(run.ID)
	if got.ReviewReadySince != nil {
		t.Errorf("ReviewReadySince = %d after head advance, want nil (rearm)", *got.ReviewReadySince)
	}

	// After rearming, a later ready re-stamps for the new head.
	if err := d.MarkRunReviewReady(run.ID); err != nil {
		t.Fatalf("re-mark review ready: %v", err)
	}
	got, _ = d.GetRun(run.ID)
	if got.ReviewReadySince == nil {
		t.Fatal("ReviewReadySince = nil after re-stamp on new head, want a timestamp")
	}
}
