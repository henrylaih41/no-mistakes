package pipeline

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FixRoundCapFindingID is the reserved finding that parks Review on the
// ordinary approval gate once review.max_fix_rounds fix rounds have run and
// findings still remain. It is keyed by ID, like the protected-path refusal:
// Respond refuses a fix on that gate unless it carries a fix-override reason,
// and every automatic resolver stands aside (HasFixRoundCap).
//
// The finding lives only in the parked gate's findings. It never joins a
// round's persisted findings or the outstanding set, and it is never handed
// to the fixer.
const FixRoundCapFindingID = "review-fix-round-cap"

// HasFixRoundCap identifies a Review gate parked at review.max_fix_rounds.
func HasFixRoundCap(findingsJSON string) bool {
	return hasFindingID(findingsJSON, FixRoundCapFindingID)
}

// reviewFixRoundCap reports whether Review has used its configured fix rounds.
// The count comes from the persisted rounds, so it survives a daemon restart,
// and it counts every fix round, automatic or user-selected. Zero disables
// the cap.
func (e *Executor) reviewFixRoundCap(stepName types.StepName, stepResultID string) (reached bool, fixRounds, maxFixRounds int, err error) {
	if stepName != types.StepReview || e.config == nil || e.config.Review.MaxFixRounds <= 0 {
		return false, 0, 0, nil
	}
	stats, err := e.db.StepRoundStats(stepResultID)
	if err != nil {
		return false, 0, 0, fmt.Errorf("count review fix rounds: %w", err)
	}
	maxFixRounds = e.config.Review.MaxFixRounds
	return stats.FixRounds >= maxFixRounds, stats.FixRounds, maxFixRounds, nil
}

// withFixRoundCapFinding appends the reserved cap finding to a gate that is
// about to park with fixable residuals. Residuals are the findings a fix
// could act on: actionable, not follow-ups, and not review questions, which
// an answer settles rather than a fix. With no residual the gate is left as
// it is.
func withFixRoundCapFinding(findingsJSON string, fixRounds, maxFixRounds int) (string, bool) {
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return findingsJSON, false
	}
	var residual []string
	for _, f := range findings.Items {
		if f.ID == "" || f.IsFollowUp() || f.ActionOrDefault() == types.ActionNoOp ||
			f.Category == types.FindingCategoryReviewQuestion || f.ID == ReviewQuestionsUnreadableFindingID {
			continue
		}
		residual = append(residual, f.ID)
	}
	if len(residual) == 0 {
		return findingsJSON, false
	}
	findings.Items = append(findings.Items, types.Finding{
		ID:       FixRoundCapFindingID,
		Severity: types.FindingSeverityError,
		Action:   types.ActionAskUser,
		Description: fmt.Sprintf("review.max_fix_rounds (%d) is reached: %d fix rounds ran and %s still remain. "+
			"Decide how to proceed: approve, skip, or abort as usual, or run one more fix round with "+
			"`no-mistakes axi respond --action fix --findings <ids> --fix-override --override-reason \"<why>\"`.",
			maxFixRounds, fixRounds, strings.Join(residual, ", ")),
	})
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return findingsJSON, false
	}
	return encoded, true
}

// withoutFixRoundCapFinding removes the reserved cap finding, for the paths
// that rebuild review state from a recovered gate's findings.
func withoutFixRoundCapFinding(findingsJSON string) string {
	if !HasFixRoundCap(findingsJSON) {
		return findingsJSON
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return findingsJSON
	}
	kept := findings.Items[:0]
	for _, f := range findings.Items {
		if f.ID != FixRoundCapFindingID {
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	findings.Items = kept
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return findingsJSON
	}
	return encoded
}

// isRoundGateFindings reports whether a parked gate's findings are its latest
// round's, allowing only the reserved cap finding the gate adds on top.
func isRoundGateFindings(roundJSON, gateJSON string) bool {
	if roundJSON == gateJSON {
		return true
	}
	if !HasFixRoundCap(gateJSON) {
		return false
	}
	round, err := types.ParseFindingsJSON(roundJSON)
	if err != nil {
		return false
	}
	canonical, err := types.MarshalFindingsJSON(round)
	return err == nil && withoutFixRoundCapFinding(gateJSON) == canonical
}

// recordFixOverride persists a fix selected past review.max_fix_rounds with
// its reason. It fails rather than let the extra round run unattributed.
func (e *Executor) recordFixOverride(roundID string, selectedIDs []string, userFindingsJSON *string, reason string) error {
	if roundID == "" {
		return fmt.Errorf("cannot record the fix override reason: the gate's round was not persisted")
	}
	var selected *string
	if idsJSON := marshalFindingIDs(selectedIDs); idsJSON != "" {
		selected = &idsJSON
	}
	if err := e.db.SetStepRoundFixOverride(roundID, selected, userFindingsJSON, reason); err != nil {
		return fmt.Errorf("record the fix override reason: %w", err)
	}
	return nil
}
