package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestParseGrokEventsSurfacesUsageAndReviewActivity(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"grok-session","model":"grok-4.6"}`,
		`{"type":"assistant","message":{"model":"grok-4.6","content":[{"type":"tool_use","name":"Read"},{"type":"text","text":"inspecting"}]},"session_id":"grok-session"}`,
		`{"type":"assistant","message":{"model":"grok-4.6","content":[{"type":"tool_use","name":"StructuredOutput"}]},"session_id":"grok-session"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","structured_output":{"findings":[]},"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"reasoning_tokens":2}}`,
		"",
	}, "\n")
	var chunks []string
	result, err := parseGrokEvents(context.Background(), strings.NewReader(events), func(chunk string) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("parseGrokEvents() error = %v", err)
	}
	if got, want := string(result.Output), `{"findings":[]}`; got != want {
		t.Fatalf("output = %s, want %s", got, want)
	}
	if result.SessionID != "grok-session" || result.Model != "grok-4.6" {
		t.Fatalf("identity = %+v", result)
	}
	wantUsage := TokenUsage{
		InputTokens: 19, OutputTokens: 7, CacheReadTokens: 5,
		CacheCreationTokens: 3, ReasoningTokens: 2,
		Reported: true, CacheCreationReported: true,
	}
	if !reflect.DeepEqual(result.Usage, wantUsage) {
		t.Fatalf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
	if result.Metrics == nil || result.Metrics.ModelRoundtrips != 2 || result.Metrics.ToolCalls != 1 || result.Metrics.ToolCategories.Read != 1 {
		t.Fatalf("activity metrics = %+v, want 2 rounds and one repository Read", result.Metrics)
	}
	if !reflect.DeepEqual(chunks, []string{"inspecting"}) {
		t.Fatalf("chunks = %v", chunks)
	}
}

func TestGrokAgentBuildArgsWithJSONSchema(t *testing.T) {
	a := &grokAgent{bin: "grok"}
	got := a.buildArgs("review this", json.RawMessage(`{"type":"object"}`))
	want := []string{
		"--permission-mode", "bypassPermissions",
		"-p", "review this",
		"--output-format", "streaming-messages-json",
		"--json-schema", `{"type":"object"}`,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestGrokAgentBuildArgsWithPlainOutput(t *testing.T) {
	a := &grokAgent{bin: "grok"}
	got := a.buildArgs("review this", nil)
	want := []string{
		"--permission-mode", "bypassPermissions",
		"-p", "review this",
		"--output-format", "streaming-messages-json",
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
		"--output-format", "streaming-messages-json",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestGrokAgentRunCapturesStructuredOutputAndChunk(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeGrok(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"inspected"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","structured_output":{"ok":true}}'
`, "@echo off\r\necho {\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"inspected\"}]}}\r\necho {\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"done\",\"structured_output\":{\"ok\":true}}\r\n")
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
	if result.Text != "done" {
		t.Fatalf("text = %q", result.Text)
	}
	if len(chunks) != 1 || chunks[0] != "inspected" {
		t.Fatalf("chunks = %q", chunks)
	}
}

func TestGrokAgentRunUsesTerminalStructuredOutputWhenTextHasMultipleSummaries(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeGrok(t, dir, `#!/bin/sh
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"summary\":\"Investigating ...\"}{\"summary\":\"Implementing ...\"}"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","structured_output":{"summary":"Done"}}'
`, "@echo off\r\necho {\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"Investigating\"}]}}\r\necho {\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"done\",\"structured_output\":{\"summary\":\"Done\"}}\r\n")
	a := &grokAgent{bin: bin}
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        dir,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(result.Output) != `{"summary":"Done"}` {
		t.Fatalf("structured output = %s", result.Output)
	}
}

func TestGrokAgentRunOnceEmitsAssistantTextWhenStructuredOutputIsInvalid(t *testing.T) {
	dir := t.TempDir()
	progress := strings.Repeat("progress summary; ", 20)
	bin := writeFakeGrok(t, dir,
		"#!/bin/sh\nprintf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\""+progress+"\"}]}}'\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":{\"summary\":42}}'\n",
		"@echo off\r\necho {\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"progress\"}]}}\r\necho {\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":{\"summary\":42}}\r\n",
	)
	var chunks []string
	a := &grokAgent{bin: bin}
	_, err := a.runOnce(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        dir,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
		OnChunk:    func(chunk string) { chunks = append(chunks, chunk) },
	})
	if err == nil {
		t.Fatal("expected invalid structured output error")
	}
	if len(chunks) != 1 || (runtime.GOOS != "windows" && chunks[0] != progress) {
		t.Fatalf("chunks = %q, want assistant progress", chunks)
	}
}

func TestGrokAgentRunUsesWorktreeCWD(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	bin := writeFakeGrok(t, binDir,
		"#!/bin/sh\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"%s\"}\\n' \"$(pwd)\"\n",
		"@echo off\r\nfor /f \"delims=\" %%i in ('cd') do echo {\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"%%i\"}\r\n",
	)
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
