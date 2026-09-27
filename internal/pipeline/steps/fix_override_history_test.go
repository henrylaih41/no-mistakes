package steps

import (
	"strings"
	"testing"
)

// A fix chosen past review.max_fix_rounds is still the user's decision, so the
// reviewer must see it as one: chosen and unchosen findings both render.
func TestRoundHistoryPromptSection_FixOverrideIsAHumanDecision(t *testing.T) {
	sctx, stepID := newRoundHistoryContext(t)

	findings := `{"findings":[{"id":"review-1","severity":"error","description":"residual crash","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"naming nit","action":"ask-user"}],"summary":"2"}`
	round, err := sctx.DB.InsertStepRound(stepID, 1, "initial", &findings, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["review-1"]`
	if err := sctx.DB.SetStepRoundFixOverride(round.ID, &selected, nil, "the crash is real"); err != nil {
		t.Fatal(err)
	}

	got := stepRoundHistorySection(sctx)
	fixedAt := strings.Index(got, "\nuser_chose_to_fix:")
	ignoredAt := strings.Index(got, "\nuser_chose_to_ignore:")
	if fixedAt < 0 || ignoredAt < 0 || ignoredAt <= fixedAt {
		t.Fatalf("override round did not render as a human decision:\n%s", got)
	}
	if fixed := got[fixedAt:ignoredAt]; !strings.Contains(fixed, `"id":"review-1"`) {
		t.Fatalf("user fix block is missing the overridden finding:\n%s", fixed)
	}
	if ignored := got[ignoredAt:]; !strings.Contains(ignored, `"id":"review-2"`) {
		t.Fatalf("user ignore block is missing the unselected finding:\n%s", ignored)
	}
}
