package steps

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type lastFixedIssues struct {
	Checks        []string `json:"checks,omitempty"`
	MergeConflict bool     `json:"mergeConflict,omitempty"`
	// HeadSHA and DevinPrints carry the post-PR review-loop anti-thrash key: the
	// commit a Devin-driven fix was made against plus the fingerprints of the
	// findings it addressed. Both are omitempty so a check/merge-conflict key
	// (the only kind produced when the review loop is disabled) marshals to
	// byte-identical JSON as before.
	HeadSHA     string   `json:"headSHA,omitempty"`
	DevinPrints []string `json:"devinPrints,omitempty"`
}

// pollInterval returns the polling interval based on elapsed time since CI monitoring started.
// 30s for first 5min, 60s for 5-15min, 120s after.
func pollInterval(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 5*time.Minute:
		return 30 * time.Second
	case elapsed < 15*time.Minute:
		return 60 * time.Second
	default:
		return 120 * time.Second
	}
}

// hasFailingChecks returns true if any CI check is in the fail bucket.
func hasFailingChecks(checks []scm.Check) bool {
	for _, c := range checks {
		if c.Failing() {
			return true
		}
	}
	return false
}

// hasPendingChecks returns true if any CI check is still running or queued.
func hasPendingChecks(checks []scm.Check) bool {
	for _, c := range checks {
		if c.Pending() {
			return true
		}
	}
	return false
}

// failingCheckNames returns the names of failing checks.
func failingCheckNames(checks []scm.Check) []string {
	var names []string
	for _, c := range checks {
		if c.Failing() {
			names = append(names, c.Name)
		}
	}
	return names
}

func failingCheckCompletionTimes(checks []scm.Check) map[string]time.Time {
	completedAt := make(map[string]time.Time)
	for _, c := range checks {
		if !c.Failing() {
			continue
		}
		if c.CompletedAt.IsZero() {
			continue
		}
		previous := completedAt[c.Name]
		if previous.IsZero() || c.CompletedAt.After(previous) {
			completedAt[c.Name] = c.CompletedAt
		}
	}
	if len(completedAt) == 0 {
		return nil
	}
	return completedAt
}

func failingCheckCompletedAfter(checks []scm.Check, after map[string]time.Time) bool {
	if len(after) == 0 {
		return false
	}
	for _, c := range checks {
		if !c.Failing() || c.CompletedAt.IsZero() {
			continue
		}
		previous, ok := after[c.Name]
		if ok && c.CompletedAt.After(previous) {
			return true
		}
	}
	return false
}

func pendingCheckMatchesLastFixed(checks []scm.Check, lastFixedChecks string) bool {
	issues, ok := decodeLastFixedChecks(lastFixedChecks)
	if !ok {
		return false
	}

	failedNames := map[string]struct{}{}
	for _, name := range issues.Checks {
		if name == "" {
			continue
		}
		failedNames[name] = struct{}{}
	}
	if len(failedNames) == 0 {
		return issues.MergeConflict && hasPendingChecks(checks)
	}

	for _, c := range checks {
		if !c.Pending() {
			continue
		}
		if _, ok := failedNames[c.Name]; ok {
			return true
		}
	}

	return false
}

func encodeLastFixedChecks(failing []string, mergeConflict bool) string {
	if len(failing) == 0 && !mergeConflict {
		return ""
	}
	encoded, err := json.Marshal(lastFixedIssues{Checks: failing, MergeConflict: mergeConflict})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// encodeDevinFixKey builds the anti-thrash key for a post-PR review-loop fix
// round. It folds the head SHA and the Devin finding fingerprints into the key
// so a fix is treated as "already attempted" only until the head advances (a new
// commit Devin must re-review) or the set of findings changes. Returns "" when
// there is nothing to key on.
func encodeDevinFixKey(failing []string, mergeConflict bool, headSHA string, devinPrints []string) string {
	if len(failing) == 0 && !mergeConflict && len(devinPrints) == 0 {
		return ""
	}
	encoded, err := json.Marshal(lastFixedIssues{
		Checks:        failing,
		MergeConflict: mergeConflict,
		HeadSHA:       headSHA,
		DevinPrints:   devinPrints,
	})
	if err != nil {
		return ""
	}
	return string(encoded)
}

func decodeLastFixedChecks(raw string) (lastFixedIssues, bool) {
	if raw == "" {
		return lastFixedIssues{}, false
	}
	var issues lastFixedIssues
	if err := json.Unmarshal([]byte(raw), &issues); err != nil {
		return lastFixedIssues{}, false
	}
	if len(issues.Checks) == 0 && !issues.MergeConflict && len(issues.DevinPrints) == 0 {
		return lastFixedIssues{}, false
	}
	return issues, true
}

func ciFailureOutcome(failing []string, mergeConflict bool, summary string) *pipeline.StepOutcome {
	findings := Findings{Summary: summary}
	for _, name := range failing {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: fmt.Sprintf("CI check failing: %s", name),
		})
	}
	if mergeConflict {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: "PR has merge conflicts with the base branch",
		})
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

// devinSeverityToFinding maps the review bot's coarse severity bucket
// (high/medium/low, as parsed from comment bodies in the github read layer) onto
// the pipeline's finding severities (error/warning/info). The rest of the
// codebase ranks and gates on error/warning/info (see types.SeverityRank and
// hasBlockingFindings); an unmapped high/medium/low would rank 0 and not count
// as blocking, so an escalated Devin finding would neither sort nor gate
// correctly. Anything unrecognized (including empty) defaults to warning so it
// still blocks.
func devinSeverityToFinding(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "high":
		return "error"
	case "medium":
		return "warning"
	case "low":
		return "info"
	default:
		return "warning"
	}
}

// devinFailureOutcome escalates an unresolved post-PR review-loop state to the
// human approval gate, surfacing the bot's outstanding findings as actionable
// items. Used when the loop exhausts its bounded rounds with Devin still
// requesting changes.
func devinFailureOutcome(findings []scm.ReviewComment, summary string) *pipeline.StepOutcome {
	out := Findings{Summary: summary}
	for _, f := range findings {
		if f.Path == "" {
			continue // top-level summary, not an actionable file-scoped finding
		}
		out.Items = append(out.Items, Finding{
			Severity:    devinSeverityToFinding(f.Severity),
			Description: fmt.Sprintf("%s:%d %s", f.Path, f.Line, f.Body),
			Action:      types.ActionAskUser,
		})
	}
	if len(out.Items) == 0 {
		out.Items = append(out.Items, Finding{
			Severity:    "warning",
			Description: "Devin still requested changes when the review loop exhausted its rounds",
			Action:      types.ActionAskUser,
		})
	}
	findingsJSON, _ := json.Marshal(out)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciMergeabilityOutcome(summary, description string) *pipeline.StepOutcome {
	findings := Findings{
		Summary: summary,
		Items: []Finding{{
			Severity:    "warning",
			Description: description,
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciMonitoringTimeoutOutcome() *pipeline.StepOutcome {
	findings := Findings{
		Summary: "CI monitoring timed out before PR was merged or closed",
		Items: []Finding{{
			Severity:    "warning",
			Description: "PR was still open when CI monitoring timed out",
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}
