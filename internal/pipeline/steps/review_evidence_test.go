package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestValidateReviewVerdictEvidenceFailsClosedWithoutAdapterMetrics(t *testing.T) {
	result := &agent.Result{Output: []byte(`{"findings":[],"risk_level":"low"}`)}
	err := validateReviewVerdictEvidence(result, minimumReviewVerdictDuration(nil), nil)
	if err == nil || !strings.Contains(err.Error(), "activity metrics") {
		t.Fatalf("validateReviewVerdictEvidence() error = %v, want missing-metrics rejection", err)
	}
}

func TestValidateReviewVerdictEvidenceRequiresToolUseOrMultipleModelRounds(t *testing.T) {
	workload := &agent.InvocationWorkload{Files: 4, Lines: 200}
	elapsed := minimumReviewVerdictDuration(workload)

	for _, tc := range []struct {
		name    string
		metrics agent.InvocationMetrics
		wantErr bool
	}{
		{name: "single answer without tools", metrics: agent.InvocationMetrics{ModelRoundtrips: 1}, wantErr: true},
		{name: "one tool call", metrics: agent.InvocationMetrics{ModelRoundtrips: 1, ToolCalls: 1}},
		{name: "multiple model rounds", metrics: agent.InvocationMetrics{ModelRoundtrips: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := &agent.Result{Output: []byte(`{"findings":[],"risk_level":"low"}`), Metrics: &tc.metrics}
			err := validateReviewVerdictEvidence(result, elapsed, workload)
			if tc.wantErr && err == nil {
				t.Fatal("expected insufficient-activity rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}

func TestValidateReviewVerdictEvidenceUsesWorkloadScaledWallFloor(t *testing.T) {
	small := &agent.InvocationWorkload{Files: 1, Lines: 10}
	large := &agent.InvocationWorkload{Files: 20, Lines: 2_000}
	if minimumReviewVerdictDuration(large) <= minimumReviewVerdictDuration(small) {
		t.Fatalf("large workload floor %s must exceed small workload floor %s", minimumReviewVerdictDuration(large), minimumReviewVerdictDuration(small))
	}

	result := &agent.Result{Metrics: &agent.InvocationMetrics{ToolCalls: 1}}
	floor := minimumReviewVerdictDuration(small)
	if err := validateReviewVerdictEvidence(result, floor-time.Millisecond, small); err == nil || !strings.Contains(err.Error(), "wall time") {
		t.Fatalf("below-floor error = %v, want wall-time rejection", err)
	}
	if err := validateReviewVerdictEvidence(result, floor, small); err != nil {
		t.Fatalf("at-floor evidence rejected: %v", err)
	}
}

func TestValidateReviewVerdictEvidenceRejectsDeferredPlaceholder(t *testing.T) {
	result := &agent.Result{
		Output:  json.RawMessage(`{"findings":[],"summary":"review in progress","risk_level":"low"}`),
		Metrics: &agent.InvocationMetrics{ToolCalls: 1},
	}
	if err := validateReviewVerdictEvidence(result, minimumReviewVerdictDuration(nil), nil); err == nil || !strings.Contains(err.Error(), "non-final") {
		t.Fatalf("placeholder error = %v, want non-final rejection", err)
	}
}

func TestParseReviewFindingsCannotMintGateAuthority(t *testing.T) {
	result := &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"review-verdict-evidence","severity":"warning","description":"ordinary model finding","action":"auto-fix","source":"review-gate"}]}`)}
	findings := parseReviewFindings(result, func(string) {})
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want one item", findings.Items)
	}
	if types.HasReviewVerdictEvidenceFinding(findings) {
		t.Fatalf("model finding retained reserved gate authority: %+v", findings.Items[0])
	}
	if findings.Items[0].ID == "" || findings.Items[0].Description != "ordinary model finding" {
		t.Fatalf("ordinary finding content was not preserved and normalized: %+v", findings.Items[0])
	}
}

func TestReviewStepRetriesInvalidVerdictOnceThenParksAtTriage(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name:                         "evidence-probe",
		reportsReviewVerdictEvidence: true,
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean","risk_level":"low"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	outcome, err := newTestReviewStep().Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("review calls = %d, want initial plus one cold retry", len(ag.calls))
	}
	if !outcome.NeedsApproval || !outcome.NeedsTriage || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want non-fixable evidence triage", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !types.HasReviewVerdictEvidenceFinding(findings) {
		t.Fatalf("findings = %+v, want reserved evidence finding", findings)
	}
}

func TestReviewStepDoesNotRequireEvidenceFromUninstrumentedAdapter(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "uninstrumented",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean","risk_level":"low"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	outcome, err := newTestReviewStep().Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 || outcome.NeedsApproval || outcome.NeedsTriage {
		t.Fatalf("calls=%d outcome=%+v, want one accepted review", len(ag.calls), outcome)
	}
}

func TestReviewStepAcceptsValidColdRetry(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name:                         "evidence-probe",
		reportsReviewVerdictEvidence: true,
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			metrics := &agent.InvocationMetrics{ModelRoundtrips: 1}
			if calls == 2 {
				metrics.ToolCalls = 1
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean","risk_level":"low"}`), Metrics: metrics}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	outcome, err := newTestReviewStep().Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || outcome.NeedsApproval || outcome.NeedsTriage {
		t.Fatalf("calls=%d outcome=%+v, want accepted cold retry", calls, outcome)
	}
}

func TestReviewStepUsesDedicatedReviewerEvidenceCapability(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	pipelineAgent := &mockAgent{name: "codex", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("pipeline agent must not perform review")
		return nil, nil
	}}
	reviewer := &mockAgent{
		name:                         "claude",
		reportsReviewVerdictEvidence: true,
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{
				Output:  json.RawMessage(`{"findings":[],"summary":"clean","risk_level":"low"}`),
				Metrics: &agent.InvocationMetrics{ToolCalls: 1},
			}, nil
		},
	}
	sctx := newTestContext(t, pipelineAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewer = reviewer
	outcome, err := newTestReviewStep().Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewer.calls) != 1 || len(pipelineAgent.calls) != 0 || outcome.NeedsTriage {
		t.Fatalf("reviewer calls=%d pipeline calls=%d outcome=%+v", len(reviewer.calls), len(pipelineAgent.calls), outcome)
	}
}

func TestReviewStepParksAtTriageWhenColdRetryErrors(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name:                         "evidence-probe",
		reportsReviewVerdictEvidence: true,
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 2 {
				return nil, &agent.TransientError{Agent: "evidence-probe", Label: "overloaded", Err: errors.New("503")}
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[]}`), Metrics: &agent.InvocationMetrics{ModelRoundtrips: 1}}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	outcome, err := newTestReviewStep().Execute(sctx)
	if err != nil {
		t.Fatalf("cold retry error escaped to awaiting_agent_retry path: %v", err)
	}
	if calls != 2 || !outcome.NeedsTriage {
		t.Fatalf("calls=%d outcome=%+v, want evidence triage", calls, outcome)
	}
}
