package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGrokAgentBuildArgs(t *testing.T) {
	a := &grokAgent{bin: "grok"}
	got := a.buildArgs("review this", json.RawMessage(`{"type":"object"}`))
	want := []string{
		"--permission-mode", "bypassPermissions",
		"-p", "review this",
		"--output-format", "plain",
		"--json-schema", `{"type":"object"}`,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestGrokAgentBuildArgsPrependsModelAndReasoningOverrides(t *testing.T) {
	a := &grokAgent{
		bin:       "grok",
		extraArgs: []string{"-m", "grok-code-fast-1", "--reasoning-effort", "high"},
	}
	got := a.buildArgs("review this", nil)
	want := []string{
		"-m", "grok-code-fast-1", "--reasoning-effort", "high",
		"--permission-mode", "bypassPermissions",
		"-p", "review this",
		"--output-format", "plain",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestGrokAgentRunCapturesStructuredOutputAndChunk(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeGrok(t, dir, `#!/bin/sh
printf '%s' '{"ok":true}'
`, "@echo off\r\n<nul set /p ={\"ok\":true}\r\n")
	var chunks []string
	a := &grokAgent{bin: bin}
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        dir,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		OnChunk:    func(chunk string) { chunks = append(chunks, chunk) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("structured output = %s", result.Output)
	}
	if result.Text != `{"ok":true}` {
		t.Fatalf("text = %q", result.Text)
	}
	if len(chunks) != 1 || chunks[0] != `{"ok":true}` {
		t.Fatalf("chunks = %q", chunks)
	}
}

func TestGrokAgentRunUsesWorktreeCWD(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	bin := writeFakeGrok(t, binDir, "#!/bin/sh\npwd\n", "@echo off\r\ncd\r\n")
	a := &grokAgent{bin: bin}
	result, err := a.Run(context.Background(), RunOpts{Prompt: "cwd", CWD: dir})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(result.Text))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cwd = %q, want %q", got, want)
	}
}

func TestGrokAgentRunReportsExitStderr(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeGrok(t, dir, "#!/bin/sh\necho 'provider unavailable' >&2\nexit 7\n", "@echo off\r\necho provider unavailable 1>&2\r\nexit /b 7\r\n")
	a := &grokAgent{bin: bin}
	_, err := a.runOnce(context.Background(), RunOpts{Prompt: "review", CWD: dir})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("error = %v, want stderr detail", err)
	}
}

func writeFakeGrok(t *testing.T, dir, posixScript, windowsScript string) string {
	t.Helper()
	name := "grok"
	script := posixScript
	if runtime.GOOS == "windows" {
		name = "grok.cmd"
		script = windowsScript
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake grok: %v", err)
	}
	return path
}
