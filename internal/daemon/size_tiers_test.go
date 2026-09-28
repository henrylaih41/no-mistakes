package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// cutoverSizeTierConfig is the gate config this change ships with: the codex
// reviewer's model and effort live in review_agents.reviewer, not in
// agent_args_override.codex, because a native effort pin there would win over
// the size tier's effort.
const cutoverSizeTierConfig = `agent: claude
agent_args_override:
  claude: [--model, claude-opus-5-5, --effort, high]
review_agents:
  reviewer: {agent: codex, model: gpt-6-astra, effort: xhigh}
size_tiers:
  small_max_lines: 100
  small_review_effort: high
`

func writeArgvCapturingCodex(t *testing.T, dir string) (bin, argvPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	argvPath = filepath.Join(dir, "codex-argv.txt")
	bin = filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuoteForTest(argvPath) + "\ncat >/dev/null\n" +
		"printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvPath
}

// reviewArgv runs one reviewer turn through the pipeline agent built from
// globalYAML and returns the codex argv it launched.
func reviewArgv(t *testing.T, globalYAML string, small bool) string {
	t.Helper()
	dir := t.TempDir()
	bin, argvPath := writeArgvCapturingCodex(t, dir)
	global, err := config.LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	cfg.AgentPathOverride = map[string]string{"codex": bin}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	if _, err := ag.Run(context.Background(), agent.RunOpts{Purpose: "review", SmallChange: small, Prompt: "review", CWD: dir}); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(argv)
}

func TestSizeTierEffortReachesTheCodexArgv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		small  bool
		effort string
	}{
		{"small change reviews at high", true, `model_reasoning_effort="high"`},
		{"standard change reviews at xhigh", false, `model_reasoning_effort="xhigh"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := reviewArgv(t, cutoverSizeTierConfig, tc.small)
			if !strings.Contains(argv, tc.effort+"\n") || strings.Count(argv, "model_reasoning_effort") != 1 {
				t.Fatalf("codex argv = %q, want exactly one %s", argv, tc.effort)
			}
			if !strings.Contains(argv, "-m\ngpt-6-astra\n") {
				t.Fatalf("codex argv = %q, want the reviewer model", argv)
			}
		})
	}
}

// The precedence trap: an effort pinned in agent_args_override.codex wins over
// any profile effort, so the tier cannot lower it. The small reviewer is then
// not built at all (and the daemon warns) rather than claiming an effort the
// argv does not carry.
func TestSizeTierEffortPinnedNativelyKeepsThePinnedEffort(t *testing.T) {
	pinned := `agent: claude
agent_args_override:
  codex: [-m, gpt-6-astra, -c, model_reasoning_effort="xhigh"]
review_agents:
  reviewer: {agent: codex}
`
	global, err := config.LoadGlobalFromBytes([]byte(pinned))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, why := config.Merge(global, &config.RepoConfig{}).SmallReviewerEntry(); ok || !strings.Contains(why, "agent_args_override.codex pins the reasoning effort") {
		t.Fatalf("SmallReviewerEntry ok=%v why=%q, want a refusal naming the pin", ok, why)
	}
	if argv := reviewArgv(t, pinned, true); !strings.Contains(argv, `model_reasoning_effort="xhigh"`) || strings.Contains(argv, `"high"`) {
		t.Fatalf("codex argv = %q, want the pinned xhigh only", argv)
	}
}
