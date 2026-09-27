package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// installPromptRecordingClaude puts a fake claude on PATH that records the
// prompt it receives on stdin and returns a clean review.
func installPromptRecordingClaude(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("prompt-recording fake harness is POSIX shell")
	}
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	const reply = `{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"},"usage":{"input_tokens":1,"output_tokens":1}}
`
	script := "#!/bin/sh\ncat >> \"" + promptPath + "\"\ncat <<'EOF'\n" + reply + "EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return promptPath
}

func TestCaptureAndReplayCarryTheRunsDesignContext(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	raw, err := types.MarshalDesignContextJSON(types.DesignContext{Files: []types.DesignContextFile{{
		Source:  "/machine/QUALITY.md",
		Content: "prefer deletion over new abstraction",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.SetRunDesignContext(run.ID, raw); err != nil {
		t.Fatal(err)
	}
	promptPath := installPromptRecordingClaude(t)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(cases[0].Dir, designContextCaseFile))
	if err != nil {
		t.Fatalf("captured case has no design context: %v", err)
	}
	if string(stored) != raw {
		t.Fatalf("captured design context = %s, want the run's pinned bytes %s", stored, raw)
	}

	_, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluations) != 1 || evaluations[0].Status != "completed" {
		t.Fatalf("replay = %#v", evaluations)
	}
	if evaluations[0].DesignContextSource != "captured" {
		t.Fatalf("design context source = %q, want captured", evaluations[0].DesignContextSource)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	fence := "-----BEGIN DESIGN CONTEXT: /machine/QUALITY.md-----\nprefer deletion over new abstraction\n-----END DESIGN CONTEXT: /machine/QUALITY.md-----"
	if !strings.Contains(string(prompt), fence) {
		t.Fatalf("replayed review prompt is missing the captured design context")
	}
}

// TestReplayRematerializesDesignContextForALegacyCase covers the cases
// captured before design context was recorded: the replay rebuilds it from
// the files the pinned global configuration names.
func TestReplayRematerializesDesignContextForALegacyCase(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	promptPath := installPromptRecordingClaude(t)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cases[0].Dir, designContextCaseFile)); !os.IsNotExist(err) {
		t.Fatalf("a run without design context captured a design-context file: %v", err)
	}
	charter := filepath.Join(t.TempDir(), "QUALITY.md")
	if err := os.WriteFile(charter, []byte("machine quality charter"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyGlobal := fmt.Sprintf("review_loop:\n  enabled: false\ndesign_context:\n  files:\n    - %q\n", charter)
	if err := os.WriteFile(filepath.Join(cases[0].Dir, "config", "global.yaml"), []byte(legacyGlobal), 0o644); err != nil {
		t.Fatal(err)
	}

	_, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluations) != 1 || evaluations[0].Status != "completed" {
		t.Fatalf("replay = %#v", evaluations)
	}
	if evaluations[0].DesignContextSource != "rematerialized" {
		t.Fatalf("design context source = %q, want rematerialized", evaluations[0].DesignContextSource)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "-----BEGIN DESIGN CONTEXT: "+charter+"-----\nmachine quality charter\n") {
		t.Fatalf("replayed review prompt is missing the rematerialized charter")
	}
}
