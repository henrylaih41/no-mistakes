package steps

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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
			result := &agent.Result{
				Output:  []byte(`{"findings":[],"risk_level":"low"}`),
				Metrics: &tc.metrics,
			}
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
		t.Fatalf("large workload floor %s must exceed small workload floor %s",
			minimumReviewVerdictDuration(large), minimumReviewVerdictDuration(small))
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

func TestValidateReviewVerdictEvidenceRejectsDeferredPlaceholderVerdict(t *testing.T) {
	result := &agent.Result{
		Output:  []byte(`{"findings":[],"summary":"review in progress","risk_level":"low"}`),
		Metrics: &agent.InvocationMetrics{ToolCalls: 1},
	}
	if err := validateReviewVerdictEvidence(result, minimumReviewVerdictDuration(nil), nil); err == nil || !strings.Contains(err.Error(), "non-final") {
		t.Fatalf("placeholder error = %v, want non-final rejection", err)
	}
}

func TestValidateReviewVerdictEvidenceRejectsObservedPlaceholderVerdict(t *testing.T) {
	result := &agent.Result{
		Output:  []byte(`{"findings":[],"summary":"","risk_level":"low","risk_rationale":"Placeholder until review completes."}`),
		Metrics: &agent.InvocationMetrics{ToolCalls: 1},
	}
	if err := validateReviewVerdictEvidence(result, minimumReviewVerdictDuration(nil), nil); err == nil || !strings.Contains(err.Error(), "non-final") {
		t.Fatalf("placeholder error = %v, want observed deferral rejection", err)
	}
}

func TestValidateReviewVerdictEvidenceDoesNotScanFindingDescriptionsForDeferralMarkers(t *testing.T) {
	result := &agent.Result{
		Output:  []byte(`{"findings":[{"severity":"warning","description":"The review in progress state is not persisted","action":"ask-master"}],"summary":"one defect","risk_level":"medium","risk_rationale":"state can be lost"}`),
		Metrics: &agent.InvocationMetrics{ToolCalls: 1},
	}
	if err := validateReviewVerdictEvidence(result, minimumReviewVerdictDuration(nil), nil); err != nil {
		t.Fatalf("legitimate finding rejected as a placeholder: %v", err)
	}
}
