package config

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobalReviewAgent(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent: codex\nreview:\n  agent: claude\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if cfg.Review.Agent != types.AgentClaude {
		t.Fatalf("review.agent = %q, want claude", cfg.Review.Agent)
	}

	merged := Merge(cfg, &RepoConfig{})
	if merged.Review.Agent != types.AgentClaude {
		t.Fatalf("merged review.agent = %q, want claude", merged.Review.Agent)
	}
	if merged.Agent != types.AgentCodex {
		t.Fatalf("merged pipeline agent = %q, want codex", merged.Agent)
	}
}

func TestLoadGlobalLegacySingleReviewerCompatibility(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`agent: codex
review:
  reviewers:
    - agent: claude
      args: [--model, claude-opus-5]
      path: /unused/legacy/claude
  max_parallel: 2
review_loop:
  enabled: false
  bot_login: old-bot
  max_rounds: 3
  fail_open: false
  reply_on_fix: true
  retrigger: false
  devin_api_key_file: /unused/key
  devin_review_api_key_file: /unused/review-key
  devin_org_id: old-org
`))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if cfg.Review.Agent != types.AgentClaude {
		t.Fatalf("review.agent = %q, want legacy reviewer claude", cfg.Review.Agent)
	}
}

func TestLoadGlobalRejectsUnsupportedReviewCompatibilityShapes(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "multiple legacy reviewers",
			yaml:    "review:\n  reviewers:\n    - agent: claude\n    - agent: codex\n",
			wantErr: "at most one",
		},
		{
			name:    "ambiguous preferred and legacy",
			yaml:    "review:\n  agent: claude\n  reviewers:\n    - agent: claude\n",
			wantErr: "cannot be combined",
		},
		{
			name:    "enabled removed review loop",
			yaml:    "review_loop:\n  enabled: true\n",
			wantErr: "review_loop.enabled must be false",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRepoRejectsGlobalOnlyReviewAgent(t *testing.T) {
	for _, yaml := range []string{
		"review:\n  agent: claude\n",
		"review:\n  reviewers:\n    - agent: claude\n",
		"review:\n  max_parallel: 1\n",
	} {
		_, err := LoadRepoFromBytes([]byte(yaml))
		if err == nil || !strings.Contains(err.Error(), "global-only") {
			t.Fatalf("LoadRepoFromBytes(%q) error = %v, want global-only", yaml, err)
		}
	}
}

func TestResolveReviewAgentDoesNotMutatePipelineAgent(t *testing.T) {
	cfg := Merge(&GlobalConfig{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex},
		Review: ReviewRaw{Agent: types.AgentClaude},
	}, &RepoConfig{})

	err := cfg.ResolveReviewAgent(context.Background(), func(bin string) (string, error) {
		if bin == "claude" {
			return "/test/claude", nil
		}
		return "", exec.ErrNotFound
	})
	if err != nil {
		t.Fatalf("ResolveReviewAgent: %v", err)
	}
	if cfg.Review.Agent != types.AgentClaude {
		t.Fatalf("review agent = %q, want claude", cfg.Review.Agent)
	}
	if cfg.Agent != types.AgentCodex || len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex {
		t.Fatalf("pipeline agents mutated: Agent=%q Agents=%v", cfg.Agent, cfg.Agents)
	}

	cfg.Review.Agent = ""
	if err := cfg.ResolveReviewAgent(context.Background(), func(string) (string, error) {
		return "", errors.New("must not probe")
	}); err != nil {
		t.Fatalf("unset review agent should be a no-op: %v", err)
	}
}
