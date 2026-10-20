package steps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const prStyleBody = "Use short sentences.\nSay the whole picture first."

// TestPRProsePromptsCarryTheStyleFile checks that every agent turn whose
// output is reader-facing PR text receives pr.style_file inside its fence:
// the PR draft writes What Changed, Test writes the Testing paragraph in
// both live-validation modes, and Review writes the risk line. The two JSON
// fields render inline, so only their turns are told to write one paragraph;
// the other targets take their layout and length from the style.
func TestPRProsePromptsCarryTheStyleFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		step   pipeline.Step
		output string
		target string
		inline bool
		setup  func(*pipeline.StepContext)
	}{
		{name: "pr", step: &PRStep{}, output: `{"title":"feat: add bar","body":"## What Changed\n\n- add Bar()"}`, target: `the "## What Changed" body`},
		{name: "review", step: &ReviewStep{}, output: `{"findings":[],"reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"bounded","risk_scope":"source-or-external"}`, target: "risk_rationale", inline: true},
		{name: "test", step: &TestStep{}, output: passingScenarioFindingsJSON, target: "testing_summary", inline: true},
		{name: "test-live-validation-off", step: &TestStep{}, output: `{"findings":[],"summary":"ok","tested":["go test ./pkg"],"testing_summary":"ran the focused tests","artifacts":[]}`, target: "testing_summary", inline: true, setup: func(sctx *pipeline.StepContext) {
			sctx.Config.Test.LiveValidation = false
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(tc.output)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			if _, ok := tc.step.(*PRStep); ok {
				env, _ := fakeGH(t, "")
				sctx.Env = env
			}
			if tc.setup != nil {
				tc.setup(sctx)
			}
			style := filepath.Join(t.TempDir(), "SKILL.md")
			writeDesignContextFile(t, style, prStyleBody+"\n")
			sctx.Config.PR.StyleFile = style

			if _, err := tc.step.Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) == 0 {
				t.Fatal("no prompt delivered")
			}
			prompt := ag.calls[0].Prompt
			requirePromptContains(t, prompt,
				"Writing style for "+tc.target+":",
				"-----BEGIN PR STYLE: "+style+"-----\n"+prStyleBody+"\n-----END PR STYLE: "+style+"-----",
			)
			onePara := "shows " + tc.target + " inline as one paragraph"
			styleLayout := "The style's layout and length replace the layout and length rules above"
			if tc.inline {
				requirePromptContains(t, prompt, onePara)
				requirePromptOmits(t, prompt, styleLayout)
			} else {
				requirePromptOmits(t, prompt, onePara)
				requirePromptContains(t, prompt, styleLayout)
			}
		})
	}
}

func TestPRStylePromptSection_UnsetIsEmpty(t *testing.T) {
	t.Parallel()
	if got := prStylePromptSection(nil, "x", false); got != "" {
		t.Errorf("nil sctx = %q, want empty", got)
	}
	if got := prStylePromptSection(&pipeline.StepContext{Config: &config.Config{}}, "x", false); got != "" {
		t.Errorf("unset style file = %q, want empty", got)
	}
}

// The style is cosmetic: a missing, empty, or oversized file must never fail
// the step. It drops the section and says so in the step log.
func TestPRStylePromptSection_UnusableFileIsDroppedWithAWarning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.md")
	writeDesignContextFile(t, empty, " \n")
	oversized := filepath.Join(dir, "big.md")
	writeDesignContextFile(t, oversized, strings.Repeat("x", 64*1024+1))
	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "missing.md"),
		"empty":     empty,
		"oversized": oversized,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logged []string
			sctx := &pipeline.StepContext{
				Config: &config.Config{PR: config.PR{StyleFile: path}},
				Log:    func(line string) { logged = append(logged, line) },
			}
			if got := prStylePromptSection(sctx, "x", false); got != "" {
				t.Errorf("section = %q, want empty", got)
			}
			if len(logged) != 1 || !strings.Contains(logged[0], "pr.style_file ignored") {
				t.Errorf("step log = %q, want one pr.style_file warning", logged)
			}
		})
	}
}

func TestPRStep_TemplateNarrativePromptCarriesTheStyleFile(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	env, _ := fakeGH(t, "")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+filepath.Join(t.TempDir(), "body.md"))
	style := filepath.Join(t.TempDir(), "SKILL.md")
	writeDesignContextFile(t, style, prStyleBody)
	sctx.Config.PR.StyleFile = style

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected one template narrative call, got %d", len(ag.calls))
	}
	requirePromptContains(t, ag.calls[0].Prompt, "Writing style for the narrative you write into the template:", prStyleBody)
}
