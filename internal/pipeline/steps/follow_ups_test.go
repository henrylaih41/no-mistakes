package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func runReviewWithFindings(t *testing.T, findings string) (*Findings, bool) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	output := `{"findings":` + findings + `,"reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"bounded","risk_scope":"source-or-external"}`
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(output)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.Review.FixRoundMinSeverity = types.FindingSeverityWarning
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse outcome findings: %v", err)
	}
	return &parsed, outcome.NeedsApproval
}

func TestReviewStep_InfoFindingsBecomeFollowUpsAndDoNotPark(t *testing.T) {
	findings, parks := runReviewWithFindings(t, `[{"id":"r1","severity":"info","file":"feature.txt","description":"rename helper","action":"auto-fix"},{"id":"r2","severity":"info","file":"feature.txt","description":"is this flag needed","action":"ask-user"}]`)
	if parks {
		t.Fatal("an info-only review parked the run")
	}
	for _, f := range findings.Items {
		if !f.IsFollowUp() || f.Action != types.ActionNoOp {
			t.Fatalf("finding %s = action %q disposition %q, want a no-op follow-up", f.ID, f.Action, f.Disposition)
		}
	}
	if !strings.HasSuffix(findings.Items[1].Description, "(reviewer action: ask-user)") {
		t.Fatalf("follow-up lost the reviewer's original action: %q", findings.Items[1].Description)
	}
}

func TestReviewStep_WarningStillParksAndAgentCannotSelfMarkAFollowUp(t *testing.T) {
	findings, parks := runReviewWithFindings(t, `[{"id":"r1","severity":"warning","file":"feature.txt","description":"missing error check","action":"auto-fix","disposition":"follow-up"}]`)
	if !parks {
		t.Fatal("a warning finding did not park the run")
	}
	if f := findings.Items[0]; f.IsFollowUp() || f.Action != types.ActionAutoFix {
		t.Fatalf("warning = action %q disposition %q, want an ordinary auto-fix finding", f.Action, f.Disposition)
	}
}

func TestBuildPipelineSummary_GroupsFollowUpsUnderHeading(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"id":"review-1","severity":"warning","file":"pkg/foo.go","line":10,"description":"warning finding","action":"auto-fix"},{"id":"review-2","severity":"info","file":"pkg/foo.go","line":20,"description":"info follow-up","action":"no-op","disposition":"follow-up"}],"summary":"2 findings"}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted}}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}},
	}

	body, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	warningIndex := strings.Index(body, "warning finding")
	headingIndex := strings.Index(body, "### Follow-ups (not fixed in-round)")
	followUpIndex := strings.Index(body, "info follow-up")
	if warningIndex < 0 || headingIndex < 0 || followUpIndex < 0 {
		t.Fatalf("missing grouped finding content in body:\n%s", body)
	}
	if !(warningIndex < headingIndex && headingIndex < followUpIndex) {
		t.Fatalf("finding order = warning:%d heading:%d follow-up:%d, want warning, heading, follow-up\n%s", warningIndex, headingIndex, followUpIndex, body)
	}

	bitbucket, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket)
	if !strings.Contains(bitbucket, "#### Follow-ups (not fixed in-round)") {
		t.Fatalf("Bitbucket body missing level-four follow-up heading:\n%s", bitbucket)
	}
}

func TestBuildPipelineSummary_NoFollowUpsNoHeading(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"warning finding","action":"auto-fix"}],"summary":"1 finding"}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted}}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}},
	}
	body, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if strings.Contains(body, "Follow-ups") {
		t.Fatalf("body contains follow-up heading without follow-ups:\n%s", body)
	}
}

func TestRoundHistoryPromptSection_SeparatesFollowUpsFromHumanDeclines(t *testing.T) {
	sctx, stepID := newRoundHistoryContext(t)

	findings := `{"findings":[{"id":"review-1","severity":"error","description":"blocking defect","action":"ask-user"},{"id":"review-2","severity":"info","description":"low-priority note","action":"no-op","disposition":"follow-up"}],"summary":"2"}`
	round, err := sctx.DB.InsertStepRound(stepID, 1, "initial", &findings, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepRoundDeclined(round.ID); err != nil {
		t.Fatal(err)
	}

	got := stepRoundHistorySection(sctx)
	ignoredAt := strings.Index(got, "\nuser_chose_to_ignore:")
	followUpsAt := strings.Index(got, "\nfollow_ups_not_presented_for_decision:")
	if ignoredAt < 0 || followUpsAt < 0 || followUpsAt <= ignoredAt {
		t.Fatalf("expected separate declined and follow-up blocks:\n%s", got)
	}
	ignored := got[ignoredAt:followUpsAt]
	if !strings.Contains(ignored, `"id":"review-1"`) || strings.Contains(ignored, `"id":"review-2"`) {
		t.Fatalf("human decline block contains the wrong findings:\n%s", ignored)
	}
	if followUps := got[followUpsAt:]; !strings.Contains(followUps, `"id":"review-2"`) || strings.Contains(followUps, `"id":"review-1"`) {
		t.Fatalf("follow-up block contains the wrong findings:\n%s", followUps)
	}
}

func TestRoundHistoryPromptSection_SelectedFollowUpIsHumanDecision(t *testing.T) {
	sctx, stepID := newRoundHistoryContext(t)

	findings := `{"findings":[{"id":"review-1","severity":"error","description":"blocking defect","action":"ask-user"},{"id":"review-2","severity":"info","description":"selected follow-up","action":"no-op","disposition":"follow-up"}],"summary":"2"}`
	round, err := sctx.DB.InsertStepRound(stepID, 1, "initial", &findings, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["review-2"]`
	if err := sctx.DB.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	got := stepRoundHistorySection(sctx)
	fixedAt := strings.Index(got, "\nuser_chose_to_fix:")
	ignoredAt := strings.Index(got, "\nuser_chose_to_ignore:")
	if fixedAt < 0 || ignoredAt < 0 || ignoredAt <= fixedAt {
		t.Fatalf("expected selected and declined decision blocks:\n%s", got)
	}
	if fixed := got[fixedAt:ignoredAt]; !strings.Contains(fixed, `"id":"review-2"`) || strings.Contains(fixed, `"id":"review-1"`) {
		t.Fatalf("user fix block contains the wrong findings:\n%s", fixed)
	}
	if strings.Contains(got, "\nfollow_ups_not_presented_for_decision:") {
		t.Fatalf("selected follow-up was also reported as undecided:\n%s", got)
	}
}
