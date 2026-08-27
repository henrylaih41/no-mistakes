package steps

import (
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	reviewVerdictBaseDuration    = 500 * time.Millisecond
	reviewVerdictPerFileDuration = 10 * time.Millisecond
	reviewVerdictPerLineDuration = 100 * time.Microsecond
	reviewVerdictMaximumDuration = 2 * time.Second
)

// minimumReviewVerdictDuration is a bounded sanity floor, not a latency
// prediction. Activity evidence remains mandatory after the floor is met.
func minimumReviewVerdictDuration(workload *agent.InvocationWorkload) time.Duration {
	floor := reviewVerdictBaseDuration
	if workload != nil {
		files := max(workload.Files, 0)
		lines := max(workload.Lines, 0)
		floor += time.Duration(files)*reviewVerdictPerFileDuration + time.Duration(lines)*reviewVerdictPerLineDuration
	}
	return min(floor, reviewVerdictMaximumDuration)
}

func validateReviewVerdictEvidence(result *agent.Result, elapsed time.Duration, workload *agent.InvocationWorkload) error {
	return validateReviewVerdictEvidenceAtFloor(result, elapsed, minimumReviewVerdictDuration(workload))
}

// validateReviewVerdictEvidenceAtFloor rejects syntactically successful
// answers that do not prove a real review turn occurred. Unknown adapter
// metrics fail closed. A single model response requires observed tool use;
// otherwise the adapter must report multiple productive model rounds.
func validateReviewVerdictEvidenceAtFloor(result *agent.Result, elapsed, floor time.Duration) error {
	if result == nil || result.Metrics == nil {
		return fmt.Errorf("review adapter reported no activity metrics")
	}
	metrics := result.Metrics
	if metrics.ToolCalls <= 0 && metrics.ModelRoundtrips <= 1 {
		return fmt.Errorf("review showed insufficient activity: tool_calls=%d model_roundtrips=%d", metrics.ToolCalls, metrics.ModelRoundtrips)
	}
	if elapsed < floor {
		return fmt.Errorf("review wall time %s was below the %s workload floor", elapsed.Round(time.Millisecond), floor)
	}
	if reviewVerdictLooksDeferred(result) {
		return fmt.Errorf("review returned a non-final placeholder verdict")
	}
	return nil
}

func reviewVerdictLooksDeferred(result *agent.Result) bool {
	text := result.Text
	if parsed, err := types.ParseFindingsJSON(string(result.Output)); err == nil {
		text = parsed.Summary + " " + parsed.RiskRationale
	}
	text = strings.ToLower(strings.TrimSpace(text))
	for _, marker := range []string{
		"review in progress",
		"review is in progress",
		"review not final",
		"review is not final",
		"analysis in progress",
		"still reviewing",
		"until review completes",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

type reviewVerdictFailure struct {
	reviewer string
	reason   error
}

func reviewVerdictTriageOutcome(failure reviewVerdictFailure, fixSummary string) *pipeline.StepOutcome {
	findings := types.Findings{
		Items: []types.Finding{{
			ID:          types.FindingIDReviewVerdictEvidence,
			Severity:    types.FindingSeverityError,
			Description: fmt.Sprintf("The %s review verdict failed the minimum evidence contract after one cold retry (%v). Triage the reviewer or adapter before accepting this review.", failure.reviewer, failure.reason),
			Action:      types.ActionAskMaster,
			Source:      types.FindingSourceReviewGate,
			ReviewScope: types.FindingReviewScopeSource,
		}},
		Summary:       "review verdict evidence invalid after cold retry",
		RiskLevel:     "high",
		RiskRationale: "No trustworthy complete source review verdict was produced.",
		RiskScope:     types.FindingsRiskScopeSourceOrExternal,
	}
	encoded, _ := types.MarshalFindingsJSON(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		NeedsTriage:   true,
		Findings:      encoded,
		FixSummary:    fixSummary,
	}
}
