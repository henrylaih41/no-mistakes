package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAgentRetryAttributionDoesNotCountAsFixRound(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/agent-retry", "https://example.com/retry.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepTest)
	if _, err := d.InsertStepRound(step.ID, 1, RoundTriggerAgentAutoRetry, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(step.ID, 2, RoundTriggerAgentManualRetry, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	stats, err := d.StepRoundStats(step.ID)
	if err != nil || stats.AgentAutoRetries != 1 {
		t.Fatalf("auto retries = %d, %v; want 1", stats.AgentAutoRetries, err)
	}
	fixes, err := d.CountStepFixRounds(step.ID)
	if err != nil || fixes != 0 {
		t.Fatalf("fix rounds = %d, %v; want 0", fixes, err)
	}
}
