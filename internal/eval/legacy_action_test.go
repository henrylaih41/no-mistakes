package eval

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Cases captured under the retired four-level authority carry "ask-master"
// in their recorded findings and gold labels. They must read as ask-user.

func TestDecisionForRoundReadsLegacyAskMasterAsAskUser(t *testing.T) {
	findings := `{"findings":[{"id":"f1","severity":"warning","file":"main.go","line":3,"description":"scope question","action":"ask-master"}]}`
	round := &db.StepRound{FindingsJSON: &findings}
	step := &db.StepResult{Status: types.StepStatusCompleted}

	if got := decisionForRound(round, step).Action; got != decisionApprove {
		t.Fatalf("decision = %q, want %q for a completed gate over a legacy ask-master finding", got, decisionApprove)
	}
}

func TestLegacyAskMasterGoldStratifiesWithAskUser(t *testing.T) {
	legacy := Case{Labels: Labels{Findings: []FindingGold{{ID: "f1", Kind: GoldTruePositive, Severity: "error", Action: "ask-master"}}}}
	current := Case{Labels: Labels{Findings: []FindingGold{{ID: "f1", Kind: GoldTruePositive, Severity: "error", Action: "ask-user"}}}}

	if got, want := findingType(legacy), findingType(current); got != want || got != "error/ask-user" {
		t.Fatalf("legacy stratum = %q, current = %q, want both error/ask-user", got, want)
	}
}
