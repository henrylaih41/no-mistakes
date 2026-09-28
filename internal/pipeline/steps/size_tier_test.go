package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const cleanReviewForSizeTier = `{"findings":[],"reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`

// reviewWithTiers runs one Review Execute under size_tiers and returns the
// reviewer invocations' SmallChange flags, the run's shared scope and the log.
func reviewWithTiers(t *testing.T, tiers config.SizeTiers, prepare func(*pipeline.StepContext)) ([]bool, *pipeline.RunShared, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	var small []bool
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "review" {
			small = append(small, opts.SmallChange)
		}
		return &agent.Result{Output: json.RawMessage(cleanReviewForSizeTier)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.SizeTiers = tiers
	sctx.Shared = &pipeline.RunShared{}
	var logs []string
	sctx.Log = func(msg string) { logs = append(logs, msg) }
	if prepare != nil {
		prepare(sctx)
	}
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	return small, sctx.Shared, strings.Join(logs, "\n")
}

func TestReviewStep_SizeTierSelectsTheSmallReviewer(t *testing.T) {
	t.Parallel()
	t.Run("a small change runs every reviewer turn as small and logs the tier", func(t *testing.T) {
		t.Parallel()
		small, shared, logs := reviewWithTiers(t, config.SizeTiers{SmallMaxLines: 100, SmallReviewEffort: "high"}, nil)
		if len(small) != 1 || !small[0] {
			t.Fatalf("reviewer SmallChange = %v, want [true]", small)
		}
		tier, ok := shared.SizeTier()
		if !ok || !tier.Small {
			t.Fatalf("shared tier = %+v ok=%v, want small recorded", tier, ok)
		}
		if !strings.Contains(logs, "size tier: small (") {
			t.Fatalf("log missing the tier line:\n%s", logs)
		}
	})
	t.Run("a standard change keeps the reviewer's own effort", func(t *testing.T) {
		t.Parallel()
		small, _, logs := reviewWithTiers(t, config.SizeTiers{SmallMaxLines: 1, SmallReviewEffort: "high"}, func(sctx *pipeline.StepContext) {
			// A code change: the fixture's feature.txt alone is docs-only.
			if err := os.WriteFile(filepath.Join(sctx.WorkDir, "code.go"), []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, sctx.WorkDir, "add", "-A")
			gitCmd(t, sctx.WorkDir, "commit", "-m", "code")
			sctx.Run.HeadSHA = gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
		})
		if len(small) != 1 || small[0] {
			t.Fatalf("reviewer SmallChange = %v, want [false]", small)
		}
		if !strings.Contains(logs, "size tier: standard (") {
			t.Fatalf("log missing the tier line:\n%s", logs)
		}
	})
	t.Run("tiers disabled never classifies", func(t *testing.T) {
		t.Parallel()
		small, shared, logs := reviewWithTiers(t, config.SizeTiers{}, nil)
		if _, ok := shared.SizeTier(); ok || len(small) != 1 || small[0] || strings.Contains(logs, "size tier") {
			t.Fatalf("small=%v logs=%q, want no classification", small, logs)
		}
	})
	t.Run("a fix round keeps the recorded tier and never reclassifies", func(t *testing.T) {
		t.Parallel()
		small, shared, logs := reviewWithTiers(t, config.SizeTiers{SmallMaxLines: 100, SmallReviewEffort: "high"}, func(sctx *pipeline.StepContext) {
			// Recorded by the run's first review; this fix round's diff is
			// irrelevant to it.
			sctx.Shared.SetSizeTier(pipeline.SizeTier{Small: true, Lines: 7})
			sctx.Fixing = true
			sctx.PreviousFindings = `{"findings":[{"id":"r1","severity":"warning","action":"auto-fix","description":"x","file":"feature.txt"}]}`
		})
		tier, _ := shared.SizeTier()
		if tier.Lines != 7 || strings.Contains(logs, "size tier:") {
			t.Fatalf("tier = %+v logs=%q, want the recorded tier kept and no new classification", tier, logs)
		}
		for _, s := range small {
			if !s {
				t.Fatalf("reviewer SmallChange = %v, want every rereview small", small)
			}
		}
	})
}

// testWithTier runs the live-validation-off Test step with a passing
// commands.test, the given recorded tier and review risk, and counts the
// agent test turns.
func testWithTier(t *testing.T, cmds config.Commands, tier *pipeline.SizeTier, risk string) (int, string) {
	t.Helper()
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ok","tested":["agent: ran"],"testing_summary":"ok","artifacts":[]}`)}, nil
	}}
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitCIWorkflowOnlyChange(t, dir, baseSHA)
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, cmds)
	sctx.Config.Test.LiveValidation = false
	sctx.Shared = &pipeline.RunShared{}
	if tier != nil {
		sctx.Shared.SetSizeTier(*tier)
	}
	sctx.Shared.SetReviewRisk(risk)
	var logs []string
	sctx.Log = func(msg string) { logs = append(logs, msg) }
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome parks: %s", outcome.Findings)
	}
	return calls, strings.Join(logs, "\n")
}

func TestTestStep_SizeTierDecidesTheAgentTestTurn(t *testing.T) {
	t.Parallel()
	passing := config.Commands{Test: "exit 0"}
	small := &pipeline.SizeTier{Small: true, Lines: 12}
	standard := &pipeline.SizeTier{Lines: 412}
	cases := []struct {
		name      string
		cmds      config.Commands
		tier      *pipeline.SizeTier
		risk      string
		wantTurns int
		wantLog   string
	}{
		{"small, passing command, low risk ends after the command", passing, small, "low", 0, "skipping the agent test turn"},
		{"small, passing command, medium risk ends after the command", passing, small, "medium", 0, "skipping the agent test turn"},
		{"small with high review risk runs the turn", passing, small, "high", 1, "size tier small (12 lines) with high review risk, asking agent"},
		{"small with no commands.test runs the turn", config.Commands{}, small, "low", 1, "no test command configured"},
		{"standard runs the turn", passing, standard, "low", 1, "size tier standard (412 lines), asking agent"},
		{"no recorded tier keeps the untiered rule", passing, nil, "high", 0, "all tests passed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			turns, logs := testWithTier(t, tc.cmds, tc.tier, tc.risk)
			if turns != tc.wantTurns {
				t.Fatalf("agent test turns = %d, want %d; logs:\n%s", turns, tc.wantTurns, logs)
			}
			if !strings.Contains(logs, tc.wantLog) {
				t.Fatalf("log missing %q:\n%s", tc.wantLog, logs)
			}
		})
	}
}

func TestPRBody_SizeTierLine(t *testing.T) {
	t.Parallel()
	shared := &pipeline.RunShared{}
	if got := withSizeTierLine("✅ Low: clean", shared); got != "✅ Low: clean" {
		t.Fatalf("unclassified run = %q, want the risk line unchanged", got)
	}
	shared.SetSizeTier(pipeline.SizeTier{Small: true, Lines: 37})
	if got, want := withSizeTierLine("✅ Low: clean", shared), "✅ Low: clean\n\nsize tier: small (37 lines)"; got != want {
		t.Fatalf("small = %q, want %q", got, want)
	}
	standard := &pipeline.RunShared{}
	standard.SetSizeTier(pipeline.SizeTier{Lines: 412})
	if got, want := withSizeTierLine("", standard), "size tier: standard (412 lines)"; got != want {
		t.Fatalf("standard without a risk line = %q, want %q", got, want)
	}
}
