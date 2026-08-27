package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// runGrok matches the Grok Build headless contracts used by no-mistakes.
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

	enc := json.NewEncoder(os.Stdout)
	if valueAfterGrokArg(args, "--output-format") != "streaming-messages-json" {
		if hasGrokArg(args, "--json-schema") {
			_ = enc.Encode(map[string]any{
				"text":             action.textOrDefault(),
				"structuredOutput": json.RawMessage(action.structuredJSON()),
			})
		} else {
			fmt.Fprintln(os.Stdout, action.textOrDefault())
		}
		return 0
	}
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
