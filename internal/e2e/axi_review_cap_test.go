//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewCapScenario makes the review step ALWAYS return one finding, so every
// re-review after a fix round parks again. That lets the test walk the
// review.max_fix_rounds cap deterministically: initial review -> one fix round
// -> awaiting_triage. Every non-review agent invocation (the fix agent, test,
// PR body, etc.) falls through to the clean default action.
func reviewCapScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review-cap-scenario.yaml")
	// delay_ms keeps the review agent busy long enough that the step is
	// observably out of a parked gate (in "fixing") between fix rounds, so the
	// CLI's poll-based waitStepLeavesGate reliably sees the transition when a
	// re-review re-parks at the same awaiting_triage status. A real agent takes
	// seconds; the instant fake would otherwise flip states between polls.
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review still finds an issue"
    delay_ms: 1000
    structured:
      findings:
        - id: "cap-1"
          severity: error
          file: "feature.txt"
          line: 1
          description: "residual issue that keeps reappearing"
          action: ask-user
      summary: "found 1 issue"
      risk_level: medium
      risk_rationale: "residual finding requires a decision"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write review cap scenario: %v", err)
	}
	return path
}

// TestAxiReviewMaxFixRoundsTriageGate proves ruling #13 end to end through the
// real `no-mistakes axi` CLI surface: with review.max_fix_rounds: 1, the review
// step parks at awaiting_triage once the cap is consumed, a plain fix there is
// refused with a structured cap error while the gate stays parked, and only a
// --fix-override with a non-empty --override-reason is allowed one more fix
// round. It also proves --yes stops at awaiting_triage instead of overriding.
func TestAxiReviewMaxFixRoundsTriageGate(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: reviewCapScenario(t)})

	// Install the gate.
	h.CommitChange("init-cap", "seed.txt", "seed\n", "seed for cap init")
	initWorktree := h.AddWorktree("init-cap")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	// The pushed .no-mistakes.yaml caps review at one fix round. The trusted
	// default branch has allow_repo_commands: true (harness default), so this
	// pushed review block is honored.
	capConfig := "review:\n  max_fix_rounds: 1\n"

	// ---- Deliberate path: cap -> triage -> refuse plain fix -> override ----
	h.CommitChange("feature/cap", "feature.txt", "change\n", "add feature change")
	h.CommitChange("feature/cap", ".no-mistakes.yaml", capConfig, "cap review fix rounds at 1")
	fw := h.AddWorktree("feature/cap")

	// Initial review parks at awaiting_approval: zero fix rounds consumed, so
	// the cap is not yet reached.
	runOut, err := h.RunInDir(fw, "axi", "run", "--intent", "enforce the review fix-round cap")
	if err != nil {
		t.Fatalf("axi run: %v\n%s", err, runOut)
	}
	if !strings.Contains(runOut, "status: awaiting_approval") {
		t.Fatalf("initial review gate was not awaiting_approval:\n%s", runOut)
	}
	t.Logf("=== axi run (initial review gate) ===\n%s", runOut)

	// One fix round consumes the cap; the re-review parks at awaiting_triage.
	fixOut, err := h.RunInDir(fw, "axi", "respond", "--action", "fix", "--findings", "cap-1")
	if err != nil {
		t.Fatalf("first fix (allowed, consumes the cap): %v\n%s", err, fixOut)
	}
	if !strings.Contains(fixOut, "status: awaiting_triage") {
		t.Fatalf("re-review after the cap did not park at awaiting_triage:\n%s", fixOut)
	}
	t.Logf("=== axi respond --action fix (lands on the triage gate) ===\n%s", fixOut)

	if triaged := waitForStepStatus(t, h, "feature/cap", types.StepReview, types.StepStatusAwaitingTriage, 60*time.Second); triaged == nil {
		t.Fatal("expected feature/cap to park at awaiting_triage after the cap")
	}

	// axi status renders the triage gate with the master-triage note and the
	// override help. This is exactly what a driving agent reads at the cap.
	statusOut, err := h.RunInDir(fw, "axi", "status")
	if err != nil {
		t.Fatalf("axi status (triage): %v\n%s", err, statusOut)
	}
	for _, want := range []string{
		"status: awaiting_triage",
		"master triage",
		"--fix-override",
		"--override-reason",
	} {
		if !strings.Contains(statusOut, want) {
			t.Errorf("axi status at triage missing %q in:\n%s", want, statusOut)
		}
	}
	t.Logf("=== axi status (awaiting_triage gate render) ===\n%s", statusOut)

	// A plain fix at the cap is refused with a structured error naming the cap,
	// and the gate stays parked (awaiting_agent not cleared).
	refuseOut, err := h.RunInDir(fw, "axi", "respond", "--action", "fix", "--findings", "cap-1")
	if err == nil {
		t.Fatalf("plain fix at the cap succeeded, want a structured refusal:\n%s", refuseOut)
	}
	if !strings.Contains(refuseOut, "max_fix_rounds") {
		t.Errorf("refusal did not name the cap:\n%s", refuseOut)
	}
	t.Logf("=== axi respond --action fix at the cap (refused) ===\n%s", refuseOut)

	stillParked := waitForStepStatus(t, h, "feature/cap", types.StepReview, types.StepStatusAwaitingTriage, 10*time.Second)
	if stillParked == nil {
		t.Fatal("run left the triage gate after a refused plain fix")
	}
	if !stillParked.AwaitingAgent {
		t.Error("AwaitingAgent cleared by a refused plain fix; the gate must stay parked")
	}

	// An override carrying a non-empty reason permits exactly one more fix
	// round; the re-review parks at awaiting_triage again.
	overrideOut, err := h.RunInDir(fw, "axi", "respond",
		"--action", "fix", "--findings", "cap-1",
		"--fix-override", "--override-reason", "master triage: residual is merge-blocking")
	if err != nil {
		t.Fatalf("override fix with reason: %v\n%s", err, overrideOut)
	}
	if !strings.Contains(overrideOut, "status: awaiting_triage") {
		t.Fatalf("re-review after the one-round override did not park at awaiting_triage again:\n%s", overrideOut)
	}
	t.Logf("=== axi respond --action fix --fix-override --override-reason (one more round) ===\n%s", overrideOut)

	if reTriaged := waitForStepStatus(t, h, "feature/cap", types.StepReview, types.StepStatusAwaitingTriage, 60*time.Second); reTriaged == nil {
		t.Fatal("expected another awaiting_triage park after the override round")
	}

	// Approve to release the run to completion.
	approveOut, err := h.RunInDir(fw, "axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("approve at triage: %v\n%s", err, approveOut)
	}
	if !strings.Contains(approveOut, "outcome: passed") {
		t.Errorf("approve at triage did not pass the run:\n%s", approveOut)
	}
	completed := h.WaitForRun("feature/cap", 60*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("feature/cap run status = %s, want completed", completed.Status)
	}

	// ---- --yes stops at the triage gate instead of overriding ----
	h.CommitChange("feature/cap-yes", "feature.txt", "change2\n", "add feature change (yes path)")
	h.CommitChange("feature/cap-yes", ".no-mistakes.yaml", capConfig, "cap review fix rounds at 1 (yes path)")
	yw := h.AddWorktree("feature/cap-yes")

	yesOut, err := h.RunInDir(yw, "axi", "run", "--yes", "--intent", "enforce the cap under --yes")
	if err != nil {
		t.Fatalf("axi run --yes: %v\n%s", err, yesOut)
	}
	if !strings.Contains(yesOut, "status: awaiting_triage") {
		t.Fatalf("--yes did not stop at awaiting_triage:\n%s", yesOut)
	}
	if strings.Contains(yesOut, "outcome: passed") {
		t.Errorf("--yes silently drove past the triage gate to a passing outcome:\n%s", yesOut)
	}
	t.Logf("=== axi run --yes (auto-fixes once, then STOPS at the triage gate) ===\n%s", yesOut)

	if yesTriaged := waitForStepStatus(t, h, "feature/cap-yes", types.StepReview, types.StepStatusAwaitingTriage, 60*time.Second); yesTriaged == nil {
		t.Fatal("expected --yes run to park at awaiting_triage")
	}

	// Release the --yes run so it does not linger parked at shutdown.
	if out, err := h.RunInDir(yw, "axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("approve --yes run at triage: %v\n%s", err, out)
	}
	if done := h.WaitForRun("feature/cap-yes", 60*time.Second); done.Status != types.RunCompleted {
		t.Fatalf("feature/cap-yes run status = %s, want completed", done.Status)
	}
}
