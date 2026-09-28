package config

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A per-run Pi pin is immutable: its effort must reach every turn, so the pin
// outranks the size tier's reviewer effort the same way it outranks
// review_agents.
func TestApplyPiProfileOutranksTheSmallReviewerEffort(t *testing.T) {
	cfg := &Config{Agent: types.AgentPi, Agents: []types.AgentName{types.AgentPi}, SizeTiers: defaultSizeTiers()}
	if _, ok, _ := cfg.SmallReviewerEntry(); !ok {
		t.Fatal("precondition: an unpinned pi run gets a small reviewer")
	}
	if err := cfg.ApplyPiProfile(&agentcfg.PiProfile{Model: "anthropic/pinned", Effort: agentcfg.EffortMax}); err != nil {
		t.Fatal(err)
	}
	if entry, ok, why := cfg.SmallReviewerEntry(); ok || why != "" {
		t.Fatalf("SmallReviewerEntry = %+v ok=%v why=%q, want no small reviewer and no warning under a pin", entry, ok, why)
	}
	if !cfg.SizeTiers.Enabled() {
		t.Fatal("the pin must keep classification on; only the reviewer effort yields")
	}
}
