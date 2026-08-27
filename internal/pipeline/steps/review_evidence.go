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

// minimumReviewVerdictDuration is deliberately based only on bounded workload
// counts. It is a sanity floor, not a prediction of model latency: activity
// evidence remains independently mandatory even after the floor is met.
func minimumReviewVerdictDuration(workload *agent.InvocationWorkload) time.Duration {
	floor := reviewVerdictBaseDuration
	if workload != nil {
		files := max(workload.Files, 0)
		lines := max(workload.Lines, 0)
		floor += time.Duration(files)*reviewVerdictPerFileDuration + time.Duration(lines)*reviewVerdictPerLineDuration
	}
	return min(floor, reviewVerdictMaximumDuration)
}

// validateReviewVerdictEvidence rejects syntactically successful review
// answers that do not prove a real review turn occurred. Unknown adapter
// metrics fail closed. A single model response is accepted only when the
// adapter observed tool activity; otherwise multiple model round-trips are
// required. Wall time is an independent lower bound scaled by diff workload.
func validateReviewVerdictEvidence(result *agent.Result, elapsed time.Duration, workload *agent.InvocationWorkload) error {
	return validateReviewVerdictEvidenceAtFloor(result, elapsed, minimumReviewVerdictDuration(workload))
}

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
		text += " " + parsed.Summary + " " + parsed.RiskRationale
	}
	text = strings.ToLower(strings.TrimSpace(text))
	for _, marker := range []string{
		"review in progress",
		"review is in progress",
		"review not final",
		"review is not final",
		"analysis in progress",
		"still reviewing",
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

func reviewVerdictTriageOutcome(failures []reviewVerdictFailure, fixSummary string) *pipeline.StepOutcome {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, fmt.Sprintf("%s: %v", failure.reviewer, failure.reason))
	}
	detail := strings.Join(parts, "; ")
	findings := types.Findings{
		Items: []types.Finding{{
			ID:          "review-verdict-evidence",
			Severity:    "error",
			Description: "The review verdict failed the minimum evidence contract after one cold retry (" + detail + "). Triage the reviewer or adapter before accepting this review.",
			Action:      types.ActionAskMaster,
			Source:      "review-gate",
			ReviewScope: types.FindingReviewScopeSource,
		}},
		Summary:       "review verdict evidence invalid after cold retry",
		RiskLevel:     "high",
		RiskRationale: "No trustworthy source review verdict was produced.",
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
