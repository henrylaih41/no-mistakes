package main

import (
	"fmt"
	"os"
)

// runGrok matches the Grok Build headless contract used by no-mistakes: one
// prompt supplied with -p, plain text on stdout for unconstrained calls, and a
// single JSON value on stdout when --json-schema is present.
func runGrok(args []string, scenario *Scenario) int {
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

	hasSchema := hasGrokArg(args, "--json-schema")
	if hasSchema && hasGrokArg(args, "--output-format") {
		fmt.Fprintln(os.Stderr, "fakeagent: grok --json-schema conflicts with --output-format")
		return 2
	}
	if hasSchema {
		_, _ = os.Stdout.Write(append(action.structuredJSON(), '\n'))
		return 0
	}
	fmt.Fprintln(os.Stdout, action.textOrDefault())
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
