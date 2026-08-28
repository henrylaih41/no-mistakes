package daemon

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type closeCountingAgent struct{ closes int }

func (a *closeCountingAgent) Name() string { return "counting" }
func (a *closeCountingAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{}, nil
}
func (a *closeCountingAgent) Close() error { a.closes++; return nil }

func TestCloseRunAgentsClosesPipelineAndReviewer(t *testing.T) {
	pipelineAgent := &closeCountingAgent{}
	reviewer := &closeCountingAgent{}
	closeRunAgents(pipelineAgent, reviewer, nil)
	if pipelineAgent.closes != 1 || reviewer.closes != 1 {
		t.Fatalf("close counts pipeline=%d reviewer=%d, want 1 each", pipelineAgent.closes, reviewer.closes)
	}
}

func TestResumeRecoveredRunShutdownClosesPipelineAndReviewer(t *testing.T) {
	pipelineAgent := &closeCountingAgent{}
	reviewer := &closeCountingAgent{}
	m := &RunManager{}
	m.shuttingDown.Store(true)
	m.resumeRecoveredRun(recoveredRunPlan{agent: pipelineAgent, reviewAgent: reviewer})
	if pipelineAgent.closes != 1 || reviewer.closes != 1 {
		t.Fatalf("recovered cleanup counts pipeline=%d reviewer=%d, want 1 each", pipelineAgent.closes, reviewer.closes)
	}
}

func TestReviewAgentRequiredForRecovery(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/tmp/recovery-review-agent", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		status types.StepStatus
		want   bool
	}{
		{status: types.StepStatusPending, want: true},
		{status: types.StepStatusAwaitingApproval, want: true},
		{status: types.StepStatusCompleted, want: false},
		{status: types.StepStatusSkipped, want: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			run, err := database.InsertRun(repo.ID, "feature-"+string(tt.status), "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatus(step.ID, tt.status); err != nil {
				t.Fatal(err)
			}
			got, err := reviewAgentRequiredForRecovery(database, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("reviewAgentRequiredForRecovery = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewRecoveredReviewAgentDefersOnlyUnusedResolutionFailure(t *testing.T) {
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed_review", true: "pending_review"}[required], func(t *testing.T) {
			cfg := &config.Config{Review: config.Review{Agent: types.AgentClaude}}
			ag, err := newRecoveredReviewAgent(context.Background(), cfg, t.TempDir(), missing, runenv.Overlay{}, required)
			if required {
				if err == nil || !strings.Contains(err.Error(), "resolve review agent") {
					t.Fatalf("required reviewer error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unused reviewer recovery: %v", err)
			}
			if ag == nil || ag.Name() != string(types.AgentClaude) {
				t.Fatalf("deferred reviewer = %#v", ag)
			}
			if _, runErr := ag.Run(context.Background(), agent.RunOpts{}); runErr == nil || !strings.Contains(runErr.Error(), "resolve review agent") {
				t.Fatalf("deferred reviewer run error = %v", runErr)
			}
		})
	}
}
