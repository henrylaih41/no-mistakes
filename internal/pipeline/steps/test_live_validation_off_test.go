package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// liveValidationOff is the production default: the helper turns it on so the
// upstream tests keep driving the evidence turn.
func liveValidationOff(t *testing.T, ag agent.Agent, cmds config.Commands) (*TestStep, func() (*types.Findings, bool)) {
	t.Helper()
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitCIWorkflowOnlyChange(t, dir, baseSHA)
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, cmds)
	sctx.Config.Test.LiveValidation = false
	step := &TestStep{}
	return step, func() (*types.Findings, bool) {
		t.Helper()
		outcome, err := step.Execute(sctx)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		findings, err := types.ParseFindingsJSON(outcome.Findings)
		if err != nil {
			t.Fatal(err)
		}
		return &findings, outcome.NeedsApproval
	}
}

func TestTestStep_LiveValidationOffWithAPassingCommandRunsNoAgentTurn(t *testing.T) {
	t.Parallel()
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(noSurfaceCIWorkflowFindingsJSON)}, nil
	}}
	_, run := liveValidationOff(t, ag, config.Commands{Test: "exit 0"})
	findings, parks := run()
	if calls != 0 {
		t.Fatalf("agent turns = %d, want none when commands.test passes and there is no intent", calls)
	}
	if parks || findings.Verdict != "" || len(findings.Items) != 0 {
		t.Fatalf("parks=%v findings=%+v, want a clean pass with no verdict", parks, findings)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "exit 0" {
		t.Fatalf("tested = %v, want the configured command", findings.Tested)
	}
}

func TestTestStep_LiveValidationOffWithAFailingCommandParksWithoutAnAgentTurn(t *testing.T) {
	t.Parallel()
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return nil, nil
	}}
	_, run := liveValidationOff(t, ag, config.Commands{Test: "exit 3"})
	findings, parks := run()
	if calls != 0 || !parks {
		t.Fatalf("agent turns = %d parks = %v, want an immediate park on the failing command", calls, parks)
	}
	if len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryTestCommand {
		t.Fatalf("findings = %+v, want the configured-command failure", findings.Items)
	}
}

// With no commands.test (or with user intent) the fork's agent test turn
// still runs, but it is asked for no verdict and a verdict it sends anyway is
// dropped, so a docs-only or CI-only change never parks on no-surface.
func TestTestStep_LiveValidationOffAgentTurnNeverParksOnAVerdict(t *testing.T) {
	t.Parallel()
	var schema json.RawMessage
	var prompt string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		schema, prompt = opts.JSONSchema, opts.Prompt
		return &agent.Result{Output: json.RawMessage(noSurfaceCIWorkflowFindingsJSON)}, nil
	}}
	_, run := liveValidationOff(t, ag, config.Commands{})
	findings, parks := run()
	if parks {
		t.Fatalf("live validation off parked: %+v", findings.Items)
	}
	if findings.Verdict != "" || len(findings.Scenarios) != 0 {
		t.Fatalf("verdict %q and %d scenarios reached the outcome with live validation off", findings.Verdict, len(findings.Scenarios))
	}
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, key := range []string{"scenarios", "verdict"} {
		if _, ok := parsed.Properties[key]; ok {
			t.Fatalf("off-mode schema still asks for %q", key)
		}
	}
	if strings.Contains(prompt, `"verdict"`) || strings.Contains(prompt, "no-surface") {
		t.Fatal("off-mode prompt still asks for a live-validation verdict")
	}
}

func TestTestStep_LiveValidationOffStillRunsTheAgentTurnForIntent(t *testing.T) {
	t.Parallel()
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ok","tested":["go test ./pkg"],"testing_summary":"ran the focused tests","artifacts":[]}`)}, nil
	}}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Config.Test.LiveValidation = false
	sctx.UserIntent = "Show users a success screen after checkout"
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || outcome.NeedsApproval {
		t.Fatalf("agent turns = %d parks = %v, want one verdict-free turn and no park", calls, outcome.NeedsApproval)
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(findings.Tested) == 0 || findings.Tested[0] != "exit 0" {
		t.Fatalf("tested = %v, want the configured command first", findings.Tested)
	}
}
