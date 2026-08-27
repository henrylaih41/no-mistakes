package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const grokScannerMaxTokenSize = 256 * 1024 * 1024

var errGrokNoStructuredOutput = errors.New("grok returned no structured output")

// grokAgent spawns the Grok Build CLI in single-turn streaming mode for each
// invocation. Streaming messages expose usage and activity evidence that the
// old one-shot response envelope discarded.
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
	if err := shellenv.RunShellCommand(cmd); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("grok exited: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("grok exited: %w", err)
	}

	result, err := parseGrokEvents(ctx, bytes.NewReader(stdout.Bytes()), opts.OnChunk)
	if err != nil {
		return nil, fmt.Errorf("grok parse events: %w", err)
	}
	return finalizeGrokResult(result, opts.JSONSchema)
}

type grokEvent struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	Message          json.RawMessage `json:"message,omitempty"`
	Result           string          `json:"result,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	SessionID        string          `json:"session_id,omitempty"`
	Model            string          `json:"model,omitempty"`
	Usage            *grokUsage      `json:"usage,omitempty"`
	Errors           []string        `json:"errors,omitempty"`
}

type grokMessage struct {
	Model   string        `json:"model,omitempty"`
	Content []grokContent `json:"content,omitempty"`
}

type grokContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
}

type grokUsage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheReadInputTokens     int  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int  `json:"cache_creation_input_tokens"`
	ReasoningTokens          *int `json:"reasoning_tokens,omitempty"`
}

func parseGrokEvents(ctx context.Context, r io.Reader, onChunk func(string)) (*Result, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), grokScannerMaxTokenSize)
	result := &Result{Metrics: &InvocationMetrics{}}
	sawResult := false

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event grokEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		if event.SessionID != "" {
			result.SessionID = event.SessionID
		}
		if event.Model != "" && event.Model != "unknown" {
			result.Model = event.Model
			result.ModelProvider = "xai"
		}

		switch event.Type {
		case "assistant":
			var message grokMessage
			if err := json.Unmarshal(event.Message, &message); err != nil {
				return nil, fmt.Errorf("decode assistant message: %w", err)
			}
			result.Metrics.ModelRoundtrips++
			if message.Model != "" && message.Model != "unknown" {
				result.Model = message.Model
				result.ModelProvider = "xai"
			}
			for _, content := range message.Content {
				if content.Type == "tool_use" && !strings.EqualFold(content.Name, "StructuredOutput") {
					result.Metrics.ToolCalls++
					result.Metrics.ToolCategories.Add(classifyStructuredTool(content.Name))
				}
				if content.Type == "text" && content.Text != "" && onChunk != nil {
					onChunk(content.Text)
				}
			}
		case "result":
			sawResult = true
			if event.IsError || event.Subtype != "success" {
				detail := strings.Join(event.Errors, "; ")
				if detail == "" {
					detail = event.Result
				}
				return nil, fmt.Errorf("grok error: subtype=%s: %s", event.Subtype, detail)
			}
			result.Text = event.Result
			result.Output = event.StructuredOutput
			if usage := normalizedGrokUsage(event.Usage); usage.Reported {
				result.Usage = usage
				result.UsageReported = true
				result.CacheCreationReported = usage.CacheCreationReported
			}
		case "error":
			var message string
			if err := json.Unmarshal(event.Message, &message); err != nil {
				message = strings.TrimSpace(string(event.Message))
			}
			return nil, fmt.Errorf("grok error: %s", message)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !sawResult {
		return nil, fmt.Errorf("grok returned no result event")
	}
	return result, nil
}

func normalizedGrokUsage(raw *grokUsage) TokenUsage {
	if raw == nil {
		return TokenUsage{}
	}
	reasoning := 0
	if raw.ReasoningTokens != nil {
		reasoning = *raw.ReasoningTokens
	}
	if raw.InputTokens == 0 && raw.OutputTokens == 0 && raw.CacheReadInputTokens == 0 &&
		raw.CacheCreationInputTokens == 0 && reasoning == 0 {
		return TokenUsage{}
	}
	return TokenUsage{
		InputTokens:           raw.InputTokens + raw.CacheReadInputTokens + raw.CacheCreationInputTokens,
		OutputTokens:          raw.OutputTokens,
		CacheReadTokens:       raw.CacheReadInputTokens,
		CacheCreationTokens:   raw.CacheCreationInputTokens,
		ReasoningTokens:       reasoning,
		Reported:              true,
		CacheCreationReported: true,
	}
}

func finalizeGrokResult(result *Result, schema json.RawMessage) (*Result, error) {
	if result == nil {
		return nil, fmt.Errorf("grok returned no result event")
	}
	if len(schema) > 0 && (len(result.Output) == 0 || bytes.Equal(bytes.TrimSpace(result.Output), []byte("null"))) {
		return nil, errGrokNoStructuredOutput
	}
	if len(schema) > 0 {
		if err := validateStructuredOutput(result.Output, schema); err != nil {
			return nil, fmt.Errorf("grok structured output: %w", err)
		}
	}
	return result, nil
}

// buildArgs constructs the managed Grok CLI invocation. Permitted user CLI
// overrides are prepended, while prompt, output, schema, permission, and cwd
// control remain reserved by config validation.
func (a *grokAgent) buildArgs(prompt string, schema json.RawMessage) []string {
	args := make([]string, 0, len(a.extraArgs)+10)
	args = append(args, a.extraArgs...)
	args = append(args,
		"--permission-mode", "bypassPermissions",
		"-p", prompt,
		"--output-format", "streaming-messages-json",
	)
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}
	return args
}
