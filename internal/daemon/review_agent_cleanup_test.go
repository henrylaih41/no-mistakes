package daemon

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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
