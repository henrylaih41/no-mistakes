package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func runGrok(args []string, scenario *Scenario) int {
	started := time.Now()
	prompt, err := extractGrokPrompt(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: grok prompt: %v\n", err)
		return 1
	}
	logInvocation("grok", prompt, args)

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}
	waitForFakeReviewEvidence(started, prompt)

	sessionID := argAfter(args, "--resume")
	if sessionID == "" {
		sessionID = "fake-grok-session"
	}
	enc := json.NewEncoder(os.Stdout)
	content := []any{map[string]any{"type": "text", "text": action.textOrDefault()}}
	if isReviewPrompt(prompt) {
		content = append([]any{map[string]any{"type": "tool_use", "name": "Read"}}, content...)
	}
	_ = enc.Encode(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
		"model":      "grok-default",
	})
	_ = enc.Encode(map[string]any{
		"type":       "assistant",
		"session_id": sessionID,
		"message": map[string]any{
			"model":   "grok-default",
			"content": content,
		},
	})
	_ = enc.Encode(map[string]any{
		"type":              "result",
		"subtype":           "success",
		"is_error":          false,
		"result":            action.textOrDefault(),
		"structured_output": json.RawMessage(action.structuredJSON()),
		"session_id":        sessionID,
		"usage": map[string]int{
			"input_tokens":                100,
			"output_tokens":               50,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
		},
	})
	return 0
}

func extractGrokPrompt(args []string) (string, error) {
	path := argAfter(args, "--prompt-file")
	if path == "" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--prompt-file=") {
				path = strings.TrimPrefix(arg, "--prompt-file=")
				break
			}
		}
	}
	if path == "" {
		return "", fmt.Errorf("missing --prompt-file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}
