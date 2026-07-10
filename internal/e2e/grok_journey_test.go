//go:build e2e

package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestGrokAgentJourney proves the native Grok adapter at the product boundary:
// a user initializes a repo, pushes a feature branch through the gate, and the
// daemon launches Grok both as the implementation agent and as a review-panel
// member. The fake speaks Grok's real headless contract (plain stdout or one
// schema-constrained JSON object), so this catches adapter/CLI flag conflicts
// that package-level argument tests cannot.
func TestGrokAgentJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:     "grok",
		AgentArgs: []string{"-m", "grok-code-fast-1", "--reasoning-effort", "high"},
		Reviewers: []string{"claude", "grok"},
		Scenario:  reviewerPanelScenario(t),
	})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "native-grok"
	h.CommitChange(branch, "grok_feature.go", "package grokfeature\n\nfunc Enabled() bool { return true }\n", "add Grok-backed feature")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("Grok run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	invocations := h.AgentInvocations()
	var grokReview *Invocation
	var grokInvocations []Invocation
	for i := range invocations {
		inv := invocations[i]
		if inv.Agent != "grok" {
			continue
		}
		grokInvocations = append(grokInvocations, inv)
		assertGrokManagedInvocation(t, h, inv)
		if strings.Contains(inv.Prompt, "Review the code changes and return structured findings") {
			copy := inv
			grokReview = &copy
		}
	}
	if len(grokInvocations) == 0 {
		t.Fatal("expected Grok to launch for agent-backed pipeline steps")
	}
	if grokReview == nil {
		t.Fatalf("expected Grok to launch as a review-panel member; invocations:\n%s", summarisePrompts(invocations))
	}
	if !sawReviewInvocation(invocations, "claude", branch) {
		t.Fatalf("expected the companion Claude reviewer to launch; invocations:\n%s", summarisePrompts(invocations))
	}

	reviewStep, ok := findStep(run.Steps, types.StepReview)
	if !ok || reviewStep.FindingsJSON == nil {
		t.Fatal("expected persisted review-panel findings")
	}
	findings, err := types.ParseFindingsJSON(*reviewStep.FindingsJSON)
	if err != nil {
		t.Fatalf("parse persisted findings: %v\n%s", err, *reviewStep.FindingsJSON)
	}
	grokFinding, ok := findingFromSource(findings.Items, "grok")
	if !ok {
		t.Fatalf("expected a persisted Grok-attributed finding, got %s", *reviewStep.FindingsJSON)
	}
	if grokFinding.ID != "review-grok-2-1" {
		t.Fatalf("Grok finding id = %q, want review-grok-2-1", grokFinding.ID)
	}

	status, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("nm axi status: %v\n%s", err, status)
	}
	t.Logf("USER-FACING AXI STATUS\n%s", status)
	t.Logf("PERSISTED REVIEW RESULT\nsource=%s id=%s severity=%s action=%s description=%s",
		grokFinding.Source, grokFinding.ID, grokFinding.Severity, grokFinding.Action, grokFinding.Description)
	t.Logf("GROK REVIEW LAUNCH\nexecutable=%s\ncwd=%s\nargs=%s",
		grokReview.Executable, grokReview.CWD, compactGrokArgs(grokReview.Args))
}

func assertGrokManagedInvocation(t *testing.T, h *Harness, inv Invocation) {
	t.Helper()
	wantExecutable := filepath.Join(h.BinDir, "grok")
	if inv.Executable != wantExecutable {
		t.Fatalf("Grok executable = %q, want configured path override %q", inv.Executable, wantExecutable)
	}
	wantPrefix := []string{"-m", "grok-code-fast-1", "--reasoning-effort", "high"}
	if len(inv.Args) < len(wantPrefix) || strings.Join(inv.Args[:len(wantPrefix)], "\x00") != strings.Join(wantPrefix, "\x00") {
		t.Fatalf("Grok args = %q, want model/reasoning override prefix %q", inv.Args, wantPrefix)
	}
	if valueAfter(inv.Args, "--permission-mode") != "bypassPermissions" {
		t.Fatalf("Grok permission args = %q, want --permission-mode bypassPermissions", inv.Args)
	}
	if valueAfter(inv.Args, "-p") == "" {
		t.Fatalf("Grok args = %q, want managed single-turn -p prompt", inv.Args)
	}
	if valueAfter(inv.Args, "--json-schema") == "" {
		t.Fatalf("Grok args = %q, want native --json-schema structured output", inv.Args)
	}
	if containsArg(inv.Args, "--output-format") {
		t.Fatalf("Grok structured invocation must omit conflicting --output-format: %q", inv.Args)
	}
	worktreesRoot := filepath.Clean(filepath.Join(h.NMHome, "worktrees")) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(inv.CWD)+string(filepath.Separator), worktreesRoot) {
		t.Fatalf("Grok cwd = %q, want a managed run worktree under %q", inv.CWD, worktreesRoot)
	}
}

func valueAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want || strings.HasPrefix(arg, want+"=") {
			return true
		}
	}
	return false
}

func compactGrokArgs(args []string) string {
	compact := append([]string(nil), args...)
	for i := 0; i+1 < len(compact); i++ {
		switch compact[i] {
		case "-p":
			compact[i+1] = fmt.Sprintf("<review prompt: %d bytes>", len(compact[i+1]))
			i++
		case "--json-schema":
			compact[i+1] = fmt.Sprintf("<JSON schema: %d bytes>", len(compact[i+1]))
			i++
		}
	}
	return fmt.Sprintf("%q", compact)
}
