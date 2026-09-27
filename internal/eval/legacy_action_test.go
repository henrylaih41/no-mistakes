package eval

import (
	"strings"
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

func TestLegacyAskMasterPinOccupiesTheAskUserStratum(t *testing.T) {
	gold := func(id, action string, capturedAt int64) Case {
		return Case{Manifest: Manifest{ID: id, RepoFingerprint: "repo-a", CapturedAt: capturedAt, ChangedLines: 10},
			Labels: Labels{Findings: []FindingGold{{ID: "f1", Kind: GoldTruePositive, Severity: "error", Action: action}}}}
	}
	legacy := gold("legacy", "ask-master", 1)
	current := gold("current", "ask-user", 2)
	stratum := diversifiedStratum(current)
	legacyKey := strings.TrimSuffix(stratum, "error/ask-user") + "error/ask-master"
	pins := []diversifiedPin{{CaseID: legacy.ID, Stratum: legacyKey, Rank: 1, PinnedAt: 1}}

	for _, size := range []int{0, 2} {
		got := planDiversified([]Case{legacy, current}, size, pins)
		if len(got) != 1 || got[0].CaseID != legacy.ID || got[0].Stratum != stratum {
			t.Fatalf("size %d: pins = %#v, want only the legacy pin, re-keyed to the ask-user stratum", size, got)
		}
	}
}
