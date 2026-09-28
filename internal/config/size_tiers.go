package config

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	// DefaultSmallMaxLines is the changed-line count (additions + deletions)
	// below which a run's change is small.
	DefaultSmallMaxLines = 100
	// DefaultSmallReviewEffort is the reviewer effort for a small change.
	DefaultSmallReviewEffort = agentcfg.EffortHigh
)

// SizeTiers is the machine-owned size policy. A small change (docs-only, or
// fewer than SmallMaxLines changed lines) is reviewed at SmallReviewEffort and,
// with live validation off, skips the agent test turn when its test command
// passed and review risk is not high. Global-only: it chooses the effort the
// operator's credentials run at, so no pushed branch may set it.
type SizeTiers struct {
	// SmallMaxLines is the exclusive changed-line cutoff; 0 disables tiers.
	SmallMaxLines     int
	SmallReviewEffort agentcfg.Effort
}

// Enabled reports whether runs are classified at all.
func (t SizeTiers) Enabled() bool { return t.SmallMaxLines > 0 }

type sizeTiersRaw struct {
	SmallMaxLines     *int   `yaml:"small_max_lines"`
	SmallReviewEffort string `yaml:"small_review_effort"`
}

func defaultSizeTiers() SizeTiers {
	return SizeTiers{SmallMaxLines: DefaultSmallMaxLines, SmallReviewEffort: DefaultSmallReviewEffort}
}

// parseSizeTiers applies raw over the defaults. An explicitly configured
// effort is validated for the harness that serves the reviewer
// (review_agents.reviewer, else the primary agent) the same way review_agents
// validates its own effort. The default effort is not: a reviewer harness that
// cannot express effort keeps its own effort for small changes
// (SmallReviewerEntry), so the default never refuses a config.
func parseSizeTiers(raw sizeTiersRaw, reviewer types.AgentName) (SizeTiers, error) {
	tiers := defaultSizeTiers()
	if raw.SmallMaxLines != nil {
		if *raw.SmallMaxLines < 0 {
			return tiers, fmt.Errorf("size_tiers.small_max_lines must be >= 0, got %d", *raw.SmallMaxLines)
		}
		tiers.SmallMaxLines = *raw.SmallMaxLines
	}
	if strings.TrimSpace(raw.SmallReviewEffort) == "" {
		return tiers, nil
	}
	effort, err := agentcfg.ParseEffort(raw.SmallReviewEffort)
	if err != nil {
		return tiers, fmt.Errorf("size_tiers.small_review_effort: %w", err)
	}
	tiers.SmallReviewEffort = effort
	if agentcfg.Known(reviewer) {
		if err := agentcfg.Validate(reviewer, agentcfg.Profile{Effort: effort}); err != nil {
			return tiers, fmt.Errorf("invalid size_tiers.small_review_effort: %w", err)
		}
	}
	return tiers, nil
}

// SmallReviewerEntry is the review_agents entry that serves a small change's
// review turns: the configured reviewer (or the primary agent) with its effort
// replaced by SmallReviewEffort. When ok is false the change is still
// classified but reviewed at the reviewer's own effort; why is non-empty when
// that happens despite tiers being enabled, so the operator can see it.
func (c *Config) SmallReviewerEntry() (entry ReviewAgent, ok bool, why string) {
	if c == nil || !c.SizeTiers.Enabled() || c.SizeTiers.SmallReviewEffort == "" {
		return ReviewAgent{}, false, ""
	}
	entry, configured := c.ReviewAgents[RoleReviewer]
	if !configured {
		if !agentcfg.Known(c.Agent) {
			return ReviewAgent{}, false, "the primary agent is auto-detected, so there is no reviewer harness to set an effort on"
		}
		entry = ReviewAgent{Agent: c.Agent}
	}
	entry.Effort = c.SizeTiers.SmallReviewEffort
	if err := agentcfg.Validate(entry.Agent, agentcfg.Profile{Model: entry.Model, Effort: entry.Effort}); err != nil {
		return ReviewAgent{}, false, err.Error()
	}
	// agent_args_override wins over a profile effort, so a pinned effort would
	// make the small reviewer silently run at the pinned value.
	if agentcfg.EffortPinned(entry.Agent, c.AgentArgsOverride[string(entry.Agent)]) {
		return ReviewAgent{}, false, fmt.Sprintf("agent_args_override.%s pins the reasoning effort; move it to review_agents.reviewer.effort", entry.Agent)
	}
	return entry, true, ""
}
