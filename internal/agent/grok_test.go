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
	result, _, err := parseGrokEvents(context.Background(), strings.NewReader(events), func(chunk string) {
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
		Reported: true, CacheCreationReported: true, ReasoningReported: true,
	}
	if !reflect.DeepEqual(result.Usage, wantUsage) {
		t.Fatalf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
	if result.Metrics == nil || result.Metrics.ModelRoundtrips != 1 || result.Metrics.ToolCalls != 1 || result.Metrics.ToolCategories.Read != 1 {
		t.Fatalf("activity metrics = %+v, want one genuine round and one repository Read", result.Metrics)
	}
	if !reflect.DeepEqual(chunks, []string{"inspecting"}) {
		t.Fatalf("chunks = %v", chunks)
	}
}

func TestParseGrokEventsClassifiesCommandsAndPreservesUnknownInput(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status && go test ./internal/agent"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":["git","status"]}},{"type":"text","text":"kept"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[]}}`,
		"",
	}, "\n")
	var chunks []string
	result, _, err := parseGrokEvents(context.Background(), strings.NewReader(events), func(chunk string) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("parseGrokEvents() error = %v", err)
	}
	metrics := result.Metrics
	if metrics == nil || metrics.ModelRoundtrips != 2 || metrics.ToolCalls != 2 || metrics.ToolCategories.Git != 1 || metrics.ToolCategories.TestLint != 1 || metrics.ToolCategories.Other != 1 {
		t.Fatalf("activity metrics = %+v, want command categories plus one structured-name fallback", metrics)
	}
	if len(chunks) != 1 || chunks[0] != "kept" {
		t.Fatalf("assistant frame was dropped: chunks=%v", chunks)
	}
}

func TestParseGrokEventsStructuredOutputEnvelopeIsNotActivity(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"reasoning"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"StructuredOutput"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[]}}`,
		"",
	}, "\n")
	result, _, err := parseGrokEvents(context.Background(), strings.NewReader(events), nil)
	if err != nil {
		t.Fatalf("parseGrokEvents() error = %v", err)
	}
	if result.Metrics == nil || result.Metrics.ModelRoundtrips != 1 || result.Metrics.ToolCalls != 0 {
		t.Fatalf("activity metrics = %+v, want one genuine round and no repository tools", result.Metrics)
	}
}

func TestParseGrokEventsSkipsMalformedAndNonEventOutput(t *testing.T) {
	events := strings.Join([]string{
		`Grok CLI update available`,
		`{"notice":"warming cache"}`,
		`{"type":"assistant","message":"malformed"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","structured_output":{"findings":[]}}`,
		"",
	}, "\n")
	result, _, err := parseGrokEvents(context.Background(), strings.NewReader(events), nil)
	if err != nil {
		t.Fatalf("parseGrokEvents() error = %v", err)
	}
	if got := string(result.Output); got != `{"findings":[]}` {
		t.Fatalf("output = %s", got)
	}
	if result.Metrics == nil || result.Metrics.ToolCalls != 1 {
		t.Fatalf("activity metrics = %+v, want one repository tool call", result.Metrics)
	}
}

func TestGrokAgentBuildArgsForStreamingReview(t *testing.T) {
	a := &grokAgent{bin: "grok"}
	got := a.buildArgs("review this", json.RawMessage(`{"type":"object"}`), true)
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
	got := a.buildArgs("fix this", nil, false)
	want := []string{
		"--permission-mode", "bypassPermissions",
		"-p", "fix this",
		"--output-format", "plain",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestGrokAgentBuildArgsWithNonReviewJSONSchema(t *testing.T) {
	a := &grokAgent{bin: "grok"}
	got := a.buildArgs("fix this", json.RawMessage(`{"type":"object"}`), false)
	want := []string{
		"--permission-mode", "bypassPermissions",
		"-p", "fix this",
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
	got := a.buildArgs("review this", nil, true)
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
		Purpose:    "review",
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
		Purpose:    "review",
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
		Purpose:    "review",
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
		OnChunk:    func(chunk string) { chunks = append(chunks, chunk) },
	})
	if err == nil {
		t.Fatal("expected invalid structured output error")
	}
	if len(chunks) != 2 || (runtime.GOOS != "windows" && chunks[0] != progress) {
		t.Fatalf("chunks = %q, want assistant progress and raw result diagnostic", chunks)
	}
	if !strings.Contains(chunks[1], "raw result event:") || !strings.Contains(chunks[1], `"structured_output":{"summary":42}`) {
		t.Fatalf("diagnostic chunk = %q, want raw terminal result event", chunks[1])
	}
}

func TestFinalizeGrokResultAllowsNullOptionalProperties(t *testing.T) {
	result := &Result{Output: json.RawMessage(`{"summary":"clean","line":null}`)}
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"summary":{"type":"string"},"line":{"type":"integer"}},
		"required":["summary"]
	}`)
	if _, err := finalizeGrokResult(result, schema); err != nil {
		t.Fatalf("finalizeGrokResult() rejected optional null: %v", err)
	}
}

func TestNormalizedGrokUsagePreservesOptionalFieldPresence(t *testing.T) {
	cacheZero := 0
	reasoningZero := 0
	withoutOptional := normalizedGrokUsage(&grokUsage{InputTokens: 1})
	if withoutOptional.CacheCreationReported || withoutOptional.ReasoningReported {
		t.Fatalf("usage = %+v, want optional fidelity fields unknown", withoutOptional)
	}
	withZeroOptional := normalizedGrokUsage(&grokUsage{
		CacheCreationInputTokens: &cacheZero,
		ReasoningTokens:          &reasoningZero,
	})
	if !withZeroOptional.CacheCreationReported || !withZeroOptional.ReasoningReported || withZeroOptional.CacheCreationTokens != 0 || withZeroOptional.ReasoningTokens != 0 {
		t.Fatalf("usage = %+v, want reported real zeros", withZeroOptional)
	}
}

func TestGrokAgentRunNonReviewRetainsLegacyOutputShapes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	tests := []struct {
		name       string
		stdout     string
		schema     json.RawMessage
		wantText   string
		wantOutput string
	}{
		{name: "plain", stdout: "done", wantText: "done"},
		{name: "response envelope", stdout: `{"text":"working","structuredOutput":{"ok":true}}`, schema: schema, wantText: "working", wantOutput: `{"ok":true}`},
		{name: "bare structured", stdout: `{"ok":true}`, schema: schema, wantText: `{"ok":true}`, wantOutput: `{"ok":true}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := writeFakeGrok(t, dir,
				"#!/bin/sh\nprintf '%s\\n' '"+tc.stdout+"'\n",
				"@echo off\r\necho "+tc.stdout+"\r\n",
			)
			result, err := (&grokAgent{bin: bin}).Run(context.Background(), RunOpts{
				Prompt:     "fix",
				Purpose:    "review-fix",
				CWD:        dir,
				JSONSchema: tc.schema,
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Text != tc.wantText || string(result.Output) != tc.wantOutput {
				t.Fatalf("result = text %q output %s, want text %q output %s", result.Text, result.Output, tc.wantText, tc.wantOutput)
			}
			if result.Metrics != nil {
				t.Fatalf("legacy non-review metrics = %+v, want unknown", result.Metrics)
			}
		})
	}
}

func TestGrokAgentRunUsesWorktreeCWD(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	bin := writeFakeGrok(t, binDir,
		"#!/bin/sh\npwd\n",
		"@echo off\r\ncd\r\n",
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

func TestGrokAgentRunOnceEmitsBoundedStdoutOnParseFailure(t *testing.T) {
	dir := t.TempDir()
	raw := strings.Repeat("x", 240) + "TAIL_MARKER"
	bin := writeFakeGrok(t, dir,
		"#!/bin/sh\nprintf '%s\\n' '"+raw+"'\n",
		"@echo off\r\necho "+raw+"\r\n",
	)
	var chunks []string
	a := &grokAgent{bin: bin}
	_, err := a.runOnce(context.Background(), RunOpts{
		Prompt:  "review",
		CWD:     dir,
		Purpose: "review",
		OnChunk: func(chunk string) { chunks = append(chunks, chunk) },
	})
	if err == nil || !strings.Contains(err.Error(), "no result event") {
		t.Fatalf("runOnce() error = %v, want missing result event", err)
	}
	if want := outputSnippet(raw); !reflect.DeepEqual(chunks, []string{want}) {
		t.Fatalf("chunks = %q, want bounded stdout snippet %q", chunks, want)
	}
	if strings.Contains(strings.Join(chunks, ""), "TAIL_MARKER") {
		t.Fatalf("diagnostic leaked output beyond snippet bound: %q", chunks)
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
