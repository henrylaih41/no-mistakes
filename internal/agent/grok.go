package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// grokAgent spawns the Grok Build CLI in single-turn headless mode for each
// invocation. Grok emits one final response on stdout; --json-schema switches
// that response to schema-constrained JSON when structured output is required.
type grokAgent struct {
	bin       string
	extraArgs []string
}

func (a *grokAgent) Name() string { return "grok" }

func (a *grokAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "grok", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *grokAgent) Close() error { return nil }

func (a *grokAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	cmd := exec.CommandContext(ctx, a.bin, a.buildArgs(opts.Prompt, opts.JSONSchema)...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD)
	// Run in a dedicated process group so cancellation reaps Grok and any
	// subprocesses it launches, rather than leaving the worktree locked.
	shellenv.ConfigureShellCommand(cmd)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("grok exited: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("grok exited: %w", err)
	}

	text := strings.TrimSpace(stdout.String())
	if opts.OnChunk != nil && text != "" {
		opts.OnChunk(text)
	}
	return finalizeTextResult("grok", text, opts.JSONSchema, TokenUsage{})
}

// buildArgs constructs the managed Grok CLI invocation. Permitted user CLI
// overrides are prepended, while prompt, output, schema, permission, and cwd
// control remain reserved by config validation.
func (a *grokAgent) buildArgs(prompt string, schema json.RawMessage) []string {
	args := make([]string, 0, len(a.extraArgs)+8)
	args = append(args, a.extraArgs...)
	args = append(args,
		"--permission-mode", "bypassPermissions",
		"-p", prompt,
	)
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	} else {
		args = append(args, "--output-format", "plain")
	}
	return args
}
