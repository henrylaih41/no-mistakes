package steps

import (
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
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
