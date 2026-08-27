package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// runGrok matches the Grok Build headless contract used by no-mistakes: one
// prompt supplied with -p and streaming-messages-json events on stdout, with
// structured_output on the terminal result when --json-schema is present.
func runGrok(args []string, scenario *Scenario) int {
	started := time.Now()
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Fprintln(os.Stdout, "grok fakeagent")
		return 0
	}

	prompt := valueAfterGrokArg(args, "-p", "--single")
	logInvocation("grok", prompt, args)
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "fakeagent: grok prompt missing (no -p found)")
		return 2
	}

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}
	waitForFakeReviewEvidence(started, prompt)

	if valueAfterGrokArg(args, "--output-format") != "streaming-messages-json" {
		fmt.Fprintln(os.Stderr, "fakeagent: grok streaming-messages-json output required")
		return 2
	}
	enc := json.NewEncoder(os.Stdout)
	content := []any{map[string]any{"type": "text", "text": action.textOrDefault()}}
	if isReviewPrompt(prompt) {
		content = append([]any{map[string]any{"type": "tool_use", "name": "Read"}}, content...)
	}
	_ = enc.Encode(map[string]any{
		"type":       "assistant",
		"session_id": "fake-grok-session",
		"model":      "fake-grok",
		"message":    map[string]any{"model": "fake-grok", "content": content},
	})
	result := map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   false,
		"result":     action.textOrDefault(),
		"session_id": "fake-grok-session",
		"usage": map[string]int{
			"input_tokens":  100,
			"output_tokens": 50,
		},
	}
	if hasGrokArg(args, "--json-schema") {
		result["structured_output"] = json.RawMessage(action.structuredJSON())
	}
	_ = enc.Encode(result)
	return 0
}

func valueAfterGrokArg(args []string, names ...string) string {
	for i := 0; i+1 < len(args); i++ {
		for _, name := range names {
			if args[i] == name {
				return args[i+1]
			}
		}
	}
	return ""
}

func hasGrokArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}
