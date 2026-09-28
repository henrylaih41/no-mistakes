package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A per-run Pi pin is immutable: its effort must reach every turn, so the pin
// outranks the size tier's reviewer effort the same way it outranks
// review_agents.
func TestApplyPiProfileOutranksTheSmallReviewerEffort(t *testing.T) {
	cfg := &Config{Agent: types.AgentPi, Agents: []types.AgentName{types.AgentPi}, SizeTiers: defaultSizeTiers(),
		ReviewAgents: map[string]ReviewAgent{RoleReviewer: {Agent: types.AgentPi}}}
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

// Only an explicit review_agents.reviewer gets a small reviewer: a synthesized
// one would replace the primary's fallback chain with a single harness.
func TestSmallReviewerNeedsAnExplicitReviewerRole(t *testing.T) {
	cfg := &Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}, SizeTiers: defaultSizeTiers()}
	if entry, ok, why := cfg.SmallReviewerEntry(); ok || why != "" {
		t.Fatalf("SmallReviewerEntry = %+v ok=%v why=%q, want none without review_agents.reviewer", entry, ok, why)
	}
	cfg.ReviewAgents = map[string]ReviewAgent{RoleReviewer: {Agent: types.AgentCodex, Effort: agentcfg.EffortXHigh}}
	entry, ok, _ := cfg.SmallReviewerEntry()
	if !ok || entry.Agent != types.AgentCodex || entry.Effort != agentcfg.EffortHigh {
		t.Fatalf("SmallReviewerEntry = %+v ok=%v, want codex at high", entry, ok)
	}
}

// size_tiers is loaded from the global config only: defaults apply when it is
// absent, 0 disables classification, and an explicit effort is validated for
// the review_agents.reviewer harness the way review_agents validates its own.
func TestLoadGlobalSizeTiers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		yaml    string
		want    SizeTiers
		wantErr string
	}{
		{"absent uses the defaults", "agent: codex\n", SizeTiers{SmallMaxLines: 100, SmallReviewEffort: agentcfg.EffortHigh}, ""},
		{"zero disables", "size_tiers: {small_max_lines: 0}\n", SizeTiers{SmallReviewEffort: agentcfg.EffortHigh}, ""},
		{"explicit values", "review_agents:\n  reviewer: {agent: codex}\nsize_tiers: {small_max_lines: 40, small_review_effort: medium}\n", SizeTiers{SmallMaxLines: 40, SmallReviewEffort: agentcfg.EffortMedium}, ""},
		{"negative cutoff is refused", "size_tiers: {small_max_lines: -1}\n", SizeTiers{}, "size_tiers.small_max_lines must be >= 0"},
		{"unknown effort is refused", "size_tiers: {small_review_effort: turbo}\n", SizeTiers{}, "size_tiers.small_review_effort"},
		{"effort the reviewer harness cannot express is refused", "review_agents:\n  reviewer: {agent: rovodev}\nsize_tiers: {small_review_effort: high}\n", SizeTiers{}, "invalid size_tiers.small_review_effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			global, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadGlobalFromBytes error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := Merge(global, &RepoConfig{}).SizeTiers; got != tc.want {
				t.Fatalf("merged SizeTiers = %+v, want %+v", got, tc.want)
			}
		})
	}
}
