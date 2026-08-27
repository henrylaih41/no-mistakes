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

// grokAgent spawns the Grok Build CLI in single-turn mode. Review invocations
// use streaming events for activity evidence; other duties retain the
// one-shot response formats supported by older Grok versions.
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
	streamReview := opts.Purpose == "review"
	cmd := exec.CommandContext(ctx, a.bin, a.buildArgs(opts.Prompt, opts.JSONSchema, streamReview)...)
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
	if !streamReview {
		text := strings.TrimSpace(stdout.String())
		result, err := finalizeLegacyGrokResult(text, opts.JSONSchema, TokenUsage{})
		if err != nil {
			if opts.OnChunk != nil && text != "" {
				opts.OnChunk(text)
			}
			return nil, err
		}
		if opts.OnChunk != nil && result.Text != "" {
			opts.OnChunk(result.Text)
		}
		return result, nil
	}

	result, rawResultEvent, err := parseGrokEvents(ctx, bytes.NewReader(stdout.Bytes()), opts.OnChunk)
	if err != nil {
		if opts.OnChunk != nil {
			if snippet := outputSnippet(stdout.String()); snippet != "" {
				opts.OnChunk(snippet)
			}
		}
		return nil, fmt.Errorf("grok parse events: %w", err)
	}
	finalized, err := finalizeGrokResult(result, opts.JSONSchema)
	if err != nil && opts.OnChunk != nil && len(rawResultEvent) > 0 {
		opts.OnChunk(fmt.Sprintf("raw result event: %s", string(rawResultEvent)))
	}
	return finalized, err
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
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type grokUsage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheReadInputTokens     int  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	ReasoningTokens          *int `json:"reasoning_tokens,omitempty"`
}

func parseGrokEvents(ctx context.Context, r io.Reader, onChunk func(string)) (*Result, json.RawMessage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), grokScannerMaxTokenSize)
	result := &Result{Metrics: &InvocationMetrics{}}
	sawResult := false
	var rawResultEvent json.RawMessage

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event grokEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
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
				continue
			}
			if len(message.Content) > 0 && !isGrokStructuredOutputEnvelope(message) {
				result.Metrics.ModelRoundtrips++
			}
			if message.Model != "" && message.Model != "unknown" {
				result.Model = message.Model
				result.ModelProvider = "xai"
			}
			for _, content := range message.Content {
				if content.Type == "tool_use" && !strings.EqualFold(content.Name, "StructuredOutput") {
					result.Metrics.ToolCalls++
					categories := ClassifyToolCommand(structuredToolCommand(content.Input))
					if len(categories) == 0 {
						categories = []ToolCategory{classifyStructuredTool(content.Name)}
					}
					for _, category := range categories {
						result.Metrics.ToolCategories.Add(category)
					}
				}
				if content.Type == "text" && content.Text != "" && onChunk != nil {
					onChunk(content.Text)
				}
			}
		case "result":
			sawResult = true
			rawResultEvent = append(rawResultEvent[:0], line...)
			if event.IsError || event.Subtype != "success" {
				detail := strings.Join(event.Errors, "; ")
				if detail == "" {
					detail = event.Result
				}
				return nil, rawResultEvent, fmt.Errorf("grok error: subtype=%s: %s", event.Subtype, detail)
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
			return nil, rawResultEvent, fmt.Errorf("grok error: %s", message)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, rawResultEvent, err
	}
	if !sawResult {
		return nil, nil, fmt.Errorf("grok returned no result event")
	}
	return result, rawResultEvent, nil
}

func isGrokStructuredOutputEnvelope(message grokMessage) bool {
	return len(message.Content) == 1 &&
		message.Content[0].Type == "tool_use" &&
		strings.EqualFold(message.Content[0].Name, "StructuredOutput")
}

func normalizedGrokUsage(raw *grokUsage) TokenUsage {
	if raw == nil {
		return TokenUsage{}
	}
	cacheCreation := 0
	if raw.CacheCreationInputTokens != nil {
		cacheCreation = *raw.CacheCreationInputTokens
	}
	reasoning := 0
	if raw.ReasoningTokens != nil {
		reasoning = *raw.ReasoningTokens
	}
	if raw.InputTokens == 0 && raw.OutputTokens == 0 && raw.CacheReadInputTokens == 0 &&
		cacheCreation == 0 && reasoning == 0 && raw.CacheCreationInputTokens == nil && raw.ReasoningTokens == nil {
		return TokenUsage{}
	}
	return TokenUsage{
		InputTokens:           raw.InputTokens + raw.CacheReadInputTokens + cacheCreation,
		OutputTokens:          raw.OutputTokens,
		CacheReadTokens:       raw.CacheReadInputTokens,
		CacheCreationTokens:   cacheCreation,
		ReasoningTokens:       reasoning,
		Reported:              true,
		CacheCreationReported: raw.CacheCreationInputTokens != nil,
		ReasoningReported:     raw.ReasoningTokens != nil,
	}
}

type grokResponse struct {
	Text             string          `json:"text"`
	StructuredOutput json.RawMessage `json:"structuredOutput"`
}

func finalizeLegacyGrokResult(text string, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if len(schema) == 0 {
		return finalizeTextResult("grok", text, nil, usage)
	}

	var response grokResponse
	if err := json.Unmarshal([]byte(text), &response); err == nil &&
		len(response.StructuredOutput) > 0 &&
		!bytes.Equal(bytes.TrimSpace(response.StructuredOutput), []byte("null")) {
		result, err := finalizeTextResult("grok", string(response.StructuredOutput), schema, usage)
		if err != nil {
			return nil, err
		}
		result.Text = response.Text
		return result, nil
	}

	return finalizeTextResult("grok", text, schema, usage)
}

func finalizeGrokResult(result *Result, schema json.RawMessage) (*Result, error) {
	if result == nil {
		return nil, fmt.Errorf("grok returned no result event")
	}
	if len(schema) > 0 && (len(result.Output) == 0 || bytes.Equal(bytes.TrimSpace(result.Output), []byte("null"))) {
		return nil, errGrokNoStructuredOutput
	}
	if len(schema) > 0 {
		validationSchema, err := textValidationSchema(schema)
		if err != nil {
			return nil, fmt.Errorf("grok structured output schema: %w", err)
		}
		if err := validateStructuredOutput(result.Output, validationSchema); err != nil {
			return nil, fmt.Errorf("grok structured output: %w", err)
		}
	}
	return result, nil
}

// buildArgs constructs the managed Grok CLI invocation. Permitted user CLI
// overrides are prepended, while prompt, output, schema, permission, and cwd
// control remain reserved by config validation.
func (a *grokAgent) buildArgs(prompt string, schema json.RawMessage, streamReview bool) []string {
	args := make([]string, 0, len(a.extraArgs)+10)
	args = append(args, a.extraArgs...)
	args = append(args,
		"--permission-mode", "bypassPermissions",
		"-p", prompt,
	)
	if streamReview {
		args = append(args, "--output-format", "streaming-messages-json")
	}
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	} else if !streamReview {
		args = append(args, "--output-format", "plain")
	}
	return args
}
