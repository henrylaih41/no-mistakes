//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDedicatedReviewAgentJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: cleanReviewScenario(t)})
	configPath := filepath.Join(h.NMHome, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(h.BinDir, "claude")
	source := strings.Replace(string(data), "auto_fix:\n", "  claude: "+claudePath+"\nreview:\n  agent: claude\nauto_fix:\n", 1)
	if err := os.WriteFile(configPath, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/dedicated-review-agent"
	h.CommitChange(branch, "feature.txt", "dedicated reviewer\n", "exercise dedicated reviewer")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error=%v", run.Status, run.Error)
	}

	reviewCalls := 0
	pipelineCalls := 0
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, agent.ReviewPromptOpening) {
			reviewCalls++
			if invocation.Agent != "claude" {
				t.Fatalf("review invocation used %q, want claude", invocation.Agent)
			}
		} else if invocation.Agent == "codex" {
			pipelineCalls++
		}
	}
	if reviewCalls == 0 {
		t.Fatal("no review invocation was recorded")
	}
	if pipelineCalls == 0 {
		t.Fatal("no non-review pipeline invocation was recorded on codex")
	}
}
