package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDevinSeverityToFinding(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"high":    "error",
		"HIGH":    "error",
		" medium": "warning",
		"low":     "info",
		"":        "warning",
		"unknown": "warning",
	}
	for in, want := range cases {
		if got := devinSeverityToFinding(in); got != want {
			t.Errorf("devinSeverityToFinding(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDevinFailureOutcomeMapsSeveritiesAndBlocks proves escalated Devin findings
// carry the pipeline's error/warning/info severities (not the raw high/medium/low
// buckets, which SeverityRank scores 0 and hasBlockingFindings ignores).
func TestDevinFailureOutcomeMapsSeveritiesAndBlocks(t *testing.T) {
	t.Parallel()
	findings := []scm.ReviewComment{
		{Path: "a.go", Line: 12, Severity: "high", Body: "off-by-one"},
		{Path: "b.go", Line: 3, Severity: "low", Body: "nit"},
		{Severity: "high", Body: "top-level summary"}, // no path -> skipped
	}
	outcome := devinFailureOutcome(findings, "exhausted")
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected approval-gated outcome, got %+v", outcome)
	}
	var parsed Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &parsed); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("items = %d, want 2 (top-level summary skipped)", len(parsed.Items))
	}
	if parsed.Items[0].Severity != "error" {
		t.Errorf("items[0].Severity = %q, want error (mapped from high)", parsed.Items[0].Severity)
	}
	if parsed.Items[1].Severity != "info" {
		t.Errorf("items[1].Severity = %q, want info (mapped from low)", parsed.Items[1].Severity)
	}
	if parsed.Items[0].Description != "a.go:12 off-by-one" {
		t.Errorf("items[0].Description = %q, want file-scoped form", parsed.Items[0].Description)
	}
	// The high finding must now classify as blocking; under the old raw
	// high/medium/low severities it would not.
	if !hasBlockingFindings(parsed.Items) {
		t.Error("expected mapped findings to be blocking")
	}
}

func TestCITimeoutAutoResolverClearsMergedOrClosedPR(t *testing.T) {
	t.Parallel()

	for _, state := range []scm.PRState{scm.PRStateMerged, scm.PRStateClosed} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			host := &fakeReviewHost{state: state}
			resolver := ciPRClosedAutoResolver(&pipeline.StepContext{Log: func(string) {}}, host, &scm.PR{Number: "42"})
			if !resolver(context.Background()) {
				t.Fatalf("resolver returned false for PR state %s", state)
			}
		})
	}

	host := &fakeReviewHost{state: scm.PRStateOpen}
	resolver := ciPRClosedAutoResolver(&pipeline.StepContext{Log: func(string) {}}, host, &scm.PR{Number: "42"})
	if resolver(context.Background()) {
		t.Fatal("resolver returned true for an open PR")
	}
}

// TestDevinManualReviewOutcomeAutoResolvesMergedOrClosedPR is the review-claude-1-1
// regression: the body-only "manual verify" park must self-heal if the PR is
// merged or closed externally while parked, exactly like the CI-timeout gate
// (nm#11). Because every manual-review park is built by devinManualReviewOutcome,
// asserting the resolver here covers both call sites (the inline loop and
// handleDevinFixRound).
func TestDevinManualReviewOutcomeAutoResolvesMergedOrClosedPR(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{Log: func(string) {}}
	pr := &scm.PR{Number: "42"}

	for _, state := range []scm.PRState{scm.PRStateMerged, scm.PRStateClosed} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			outcome := devinManualReviewOutcome(sctx, &fakeReviewHost{state: state}, pr, "manual verify")
			if outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("expected a NeedsApproval park, got %+v", outcome)
			}
			if outcome.ApprovalAutoResolve == nil {
				t.Fatal("manual-review outcome must carry an ApprovalAutoResolve (nm#11)")
			}
			if !outcome.ApprovalAutoResolve(context.Background()) {
				t.Fatalf("auto-resolver must clear the parked gate for PR state %s", state)
			}
		})
	}

	outcome := devinManualReviewOutcome(sctx, &fakeReviewHost{state: scm.PRStateOpen}, pr, "manual verify")
	if outcome.ApprovalAutoResolve == nil {
		t.Fatal("manual-review outcome must carry an ApprovalAutoResolve even for an open PR")
	}
	if outcome.ApprovalAutoResolve(context.Background()) {
		t.Fatal("auto-resolver must not clear the gate while the PR is still open")
	}
}

// TestWithDevinManualVerify_FoldsSignalIntoCIFailureOutcome is the review-codex-2-1
// regression: when checks are failing AND Devin reported a body-only not-green
// signal, the CI-failure park must still carry the manual-verify finding so the
// not-green Devin signal is never hidden behind a CI-only gate (ruling #3). An
// empty reason (no manual-review state) must be a byte-identical no-op.
func TestWithDevinManualVerify_FoldsSignalIntoCIFailureOutcome(t *testing.T) {
	t.Parallel()

	base := ciFailureOutcome([]string{"build"}, false, "CI failures require manual intervention")
	if got := withDevinManualVerify(base, ""); got.Findings != base.Findings {
		t.Fatalf("empty reason must not alter findings:\n got %q\nwant %q", got.Findings, base.Findings)
	}

	combined := withDevinManualVerify(
		ciFailureOutcome([]string{"build"}, false, "CI failures require manual intervention"),
		cimonitor.ReviewManualVerifyMsg,
	)
	if !combined.NeedsApproval {
		t.Fatal("combined outcome must still be a NeedsApproval park")
	}
	var parsed Findings
	if err := json.Unmarshal([]byte(combined.Findings), &parsed); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if !strings.Contains(parsed.Summary, cimonitor.ReviewManualVerifyMsg) {
		t.Errorf("summary must mention the manual-verify reason, got %q", parsed.Summary)
	}
	foundCheck, foundManual := false, false
	for _, it := range parsed.Items {
		if strings.Contains(it.Description, "build") {
			foundCheck = true
		}
		if it.Description == cimonitor.ReviewManualVerifyMsg {
			foundManual = true
			if it.Action != types.ActionAskUser {
				t.Errorf("manual-verify finding action = %q, want %q", it.Action, types.ActionAskUser)
			}
		}
	}
	if !foundCheck {
		t.Error("combined outcome must still carry the failing-check finding")
	}
	if !foundManual {
		t.Error("combined outcome must carry the Devin manual-verify finding")
	}
}

func TestPendingCheckMatchesLastFixed_SpecialCheckNames(t *testing.T) {
	t.Parallel()

	lastFixedChecks := encodeLastFixedChecks([]string{"lint,unit", "deploy+conflict"}, true)
	checks := []scm.Check{
		{Name: "lint,unit", Bucket: "pending"},
	}

	if !pendingCheckMatchesLastFixed(checks, lastFixedChecks) {
		t.Fatalf("expected pending check with special characters to match encoded last fixed checks %q", lastFixedChecks)
	}

	checks = []scm.Check{
		{Name: "lint", Bucket: "pending"},
	}
	if pendingCheckMatchesLastFixed(checks, lastFixedChecks) {
		t.Fatalf("expected unrelated pending check not to match encoded last fixed checks %q", lastFixedChecks)
	}
}
