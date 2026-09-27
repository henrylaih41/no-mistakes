package pipeline

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const capResidualFindings = `{"findings":[{"id":"review-1","severity":"warning","file":"service.go","description":"missing error check","action":"auto-fix"}],"summary":"1 finding"}`

// fixRoundCapStep reports the same fixable finding on every round and
// remembers what each round was handed.
type fixRoundCapStep struct {
	mu       sync.Mutex
	previous []string
}

func (s *fixRoundCapStep) Name() types.StepName { return types.StepReview }

func (s *fixRoundCapStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	s.mu.Lock()
	s.previous = append(s.previous, sctx.PreviousFindings)
	s.mu.Unlock()
	return &StepOutcome{AutoFixable: true, NeedsApproval: true, Findings: capResidualFindings}, nil
}

func (s *fixRoundCapStep) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.previous)
}

func waitForCalls(t *testing.T, step *fixRoundCapStep, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for step.calls() < want {
		if time.Now().After(deadline) {
			t.Fatalf("review ran %d times, want %d", step.calls(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func reviewStepResult(t *testing.T, database *db.DB, runID string) *db.StepResult {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	return steps[0]
}

func TestExecutor_FixRoundCapParksForADecisionAndAFixNeedsAnOverride(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &fixRoundCapStep{}
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 5}, Review: config.Review{MaxFixRounds: 1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())

	// Round 1 auto-fixes; round 2 is the one fix round the cap allows, so the
	// auto-fix budget left over (5) must not buy a third.
	waitForCalls(t, step, 2)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if got := step.calls(); got != 2 {
		t.Fatalf("review ran %d times before the cap gate, want 2", got)
	}
	sr := reviewStepResult(t, database, run.ID)
	if sr.FindingsJSON == nil || !HasFixRoundCap(*sr.FindingsJSON) || !strings.Contains(*sr.FindingsJSON, `"review-1"`) {
		t.Fatalf("cap gate findings = %v, want review-1 plus the reserved cap finding", sr.FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err == nil || !strings.Contains(err.Error(), "--fix-override --override-reason") {
		t.Fatalf("fix without an override = %v, want a refusal naming the flags", err)
	}
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, nil, nil, "", "   "); err == nil {
		t.Fatal("a blank override reason was accepted")
	}
	const reason = "the residual error check guards a real crash"
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, nil, nil, "", reason); err != nil {
		t.Fatalf("fix with an override: %v", err)
	}

	// Exactly one more round, then the cap gate again.
	waitForCalls(t, step, 3)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if got := step.calls(); got != 3 {
		t.Fatalf("review ran %d times after one override, want 3", got)
	}
	if prev := step.previous[2]; HasFixRoundCap(prev) || !strings.Contains(prev, `"review-1"`) {
		t.Fatalf("override fix round was handed %q, want review-1 without the cap finding", prev)
	}

	rounds, err := database.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 3 {
		t.Fatalf("rounds = %d, want 3", len(rounds))
	}
	for _, r := range rounds {
		if r.FindingsJSON != nil && HasFixRoundCap(*r.FindingsJSON) {
			t.Fatalf("round %d persisted the reserved cap finding: %s", r.Round, *r.FindingsJSON)
		}
	}
	override := rounds[1]
	if override.SelectionSource == nil || *override.SelectionSource != db.RoundSelectionSourceUserOverride {
		t.Fatalf("override round selection source = %v, want %q", override.SelectionSource, db.RoundSelectionSourceUserOverride)
	}
	if override.FixOverrideReason == nil || *override.FixOverrideReason != reason {
		t.Fatalf("override round reason = %v, want %q", override.FixOverrideReason, reason)
	}
	if override.SelectedFindingIDs == nil || !strings.Contains(*override.SelectedFindingIDs, "review-1") {
		t.Fatalf("override round selection = %v, want review-1", override.SelectedFindingIDs)
	}

	// Approve still resolves the cap gate.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve at the cap gate: %v", err)
	}
	waitExecutorDone(t, done)
	if got := step.calls(); got != 3 {
		t.Fatalf("approve ran another review round: %d calls", got)
	}
}

func TestExecutor_FixOverrideIsRefusedOffTheCapGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"review-1","severity":"error","file":"service.go","description":"nil deref","action":"ask-user"}],"summary":"1"}`}, nil
	}}
	cfg := &config.Config{Review: config.Review{MaxFixRounds: 3}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if sr := reviewStepResult(t, database, run.ID); sr.FindingsJSON != nil && HasFixRoundCap(*sr.FindingsJSON) {
		t.Fatal("an ordinary gate carried the cap finding")
	}
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, nil, nil, "", "because"); err == nil || !strings.Contains(err.Error(), "review.max_fix_rounds") {
		t.Fatalf("override off the cap gate = %v, want a refusal", err)
	}
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionApprove, nil, nil, nil, "", "because"); err == nil {
		t.Fatal("an override reason was accepted with approve")
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

// TestExecutor_RecoveredCapGateStillNeedsAnOverride covers the restart path:
// Resume rebuilds the gate from the persisted findings, so the reserved
// finding alone must re-arm the refusal, stay out of the fixer's input, and
// the override must be recorded on the gate's round.
func TestExecutor_RecoveredCapGateStillNeedsAnOverride(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	roundFindings := capResidualFindings
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &roundFindings, nil, "", 10); err != nil {
		t.Fatal(err)
	}
	gateRound, err := database.InsertReviewStepRound(stepResult.ID, 2, "auto_fix", &roundFindings, nil, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	gateFindings, ok := withFixRoundCapFinding(roundFindings, 1, 1)
	if !ok {
		t.Fatal("expected a cap finding for a fixable residual")
	}
	if err := database.SetStepFindings(stepResult.ID, gateFindings); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 20); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	step := &fixRoundCapStep{}
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 5}, Review: config.Review{MaxFixRounds: 1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"})
		if err != nil && strings.Contains(err.Error(), "--fix-override") {
			break
		}
		if err == nil {
			t.Fatal("recovered cap gate accepted a fix without an override")
		}
		select {
		case resumeErr := <-done:
			t.Fatalf("resume returned before parking: %v", resumeErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered gate never parked: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	const reason = "operator ruled the residual is a real defect"
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1", FixRoundCapFindingID}, nil, nil, "", reason); err != nil {
		t.Fatalf("override on the recovered gate: %v", err)
	}
	waitForCalls(t, step, 1)
	if prev := step.previous[0]; HasFixRoundCap(prev) || !strings.Contains(prev, `"review-1"`) {
		t.Fatalf("recovered override round was handed %q, want review-1 without the cap finding", prev)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovered executor timed out")
	}

	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rounds {
		if r.ID != gateRound.ID {
			continue
		}
		if r.FixOverrideReason == nil || *r.FixOverrideReason != reason || r.SelectionSource == nil || *r.SelectionSource != db.RoundSelectionSourceUserOverride {
			t.Fatalf("recovered override round = source %v reason %v, want %q with the reason", r.SelectionSource, r.FixOverrideReason, db.RoundSelectionSourceUserOverride)
		}
		return
	}
	t.Fatal("gate round not found")
}
