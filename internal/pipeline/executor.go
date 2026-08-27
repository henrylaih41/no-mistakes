package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/gateguidance"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// EventFunc is called when a pipeline event occurs, for streaming to subscribers.
type EventFunc func(ipc.Event)

const (
	defaultGateReconcileInterval = 2 * time.Minute
	defaultGateReconcileTimeout  = 30 * time.Second
)

type approvalResponse struct {
	action            types.ApprovalAction
	findingIDs        []string
	instructions      map[string]string
	addedFindings     []types.Finding
	fixOverrideReason string
	autoRetry         bool
}

// Executor runs pipeline steps sequentially and coordinates approval interactions.
type Executor struct {
	db     *db.DB
	paths  *paths.Paths
	config *config.Config
	forge  *forgecontext.Context
	agent  agent.Agent
	steps  []Step
	skips  map[types.StepName]bool

	onEvent EventFunc

	// sessions manages this run's durable review-loop agent sessions; shared
	// carries run-scoped step-to-step results. Both are created per Execute.
	sessions *RunSessions
	shared   *RunShared
	workDir  string

	mu                   sync.Mutex
	approvalCh           chan approvalResponse // buffered channel for approval responses
	waiting              bool                  // true when blocked on approval
	waitingStep          types.StepName        // which step is currently awaiting approval
	waitingStepResultID  string
	waitingAgentRetry    bool
	waitingAtFixRoundCap bool
	waitingFixRoundCount int
	waitingMaxFixRounds  int

	gateReconcileInterval time.Duration
	gateReconcileTimeout  time.Duration
	onPRMerged            func(context.Context, string)
}

// SetOnPRMerged registers a best-effort hook invoked after a merged PR state
// is persisted. The pipeline never fails the run if the hook errors.
func (e *Executor) SetOnPRMerged(fn func(context.Context, string)) {
	if e == nil {
		return
	}
	e.onPRMerged = fn
}

// SetForgeContext configures the immutable provider context used by every
// subprocess in this run. A nil context preserves ambient behavior.
func (e *Executor) SetForgeContext(ctx *forgecontext.Context) {
	e.forge = ctx
}

// SetSkippedSteps configures steps that should be marked skipped without running.
func (e *Executor) SetSkippedSteps(steps []types.StepName) {
	if len(steps) == 0 {
		e.skips = nil
		return
	}
	e.skips = make(map[types.StepName]bool, len(steps))
	for _, step := range steps {
		e.skips[step] = true
	}
}

// NewExecutor creates a pipeline executor.
func NewExecutor(database *db.DB, p *paths.Paths, cfg *config.Config, ag agent.Agent, steps []Step, onEvent EventFunc) *Executor {
	if onEvent == nil {
		onEvent = func(ipc.Event) {}
	}
	return &Executor{
		db:                    database,
		paths:                 p,
		config:                cfg,
		agent:                 ag,
		steps:                 steps,
		onEvent:               onEvent,
		approvalCh:            make(chan approvalResponse, 1),
		gateReconcileInterval: defaultGateReconcileInterval,
		gateReconcileTimeout:  defaultGateReconcileTimeout,
	}
}

// runEvidenceDir resolves where this run's test evidence is written. The
// executor is the single owner of that answer for the pipeline: steps read it
// from StepContext rather than recomputing a path, which is what let the
// steering preamble and the test step drift apart while both hardcoded the
// system temp directory.
func (e *Executor) runEvidenceDir(runID string) string {
	if e.paths == nil {
		return ""
	}
	configured := ""
	if e.config != nil {
		configured = e.config.Test.Evidence.LocalRoot
	}
	return e.paths.RunEvidenceDir(configured, runID)
}

// SetGateReconcileTimings overrides the interval between approval-gate
// reconciliation checks and the deadline for each check. It is primarily used
// by deterministic tests and specialized embeddings; non-positive values keep
// the production defaults.
func (e *Executor) SetGateReconcileTimings(interval, timeout time.Duration) {
	if interval > 0 {
		e.gateReconcileInterval = interval
	}
	if timeout > 0 {
		e.gateReconcileTimeout = timeout
	}
}

// Respond sends a user approval action to the currently waiting step.
// The step parameter must match the step currently awaiting approval.
// Returns an error if no step is awaiting approval or if the step name doesn't match.
func (e *Executor) Respond(step types.StepName, action types.ApprovalAction, findingIDs []string) error {
	return e.RespondWithOverrides(step, action, findingIDs, nil, nil)
}

// RespondWithOverrides is like Respond but also carries per-finding user
// instructions and user-authored findings. Both are merged into the round's
// findings on a fix action before the fix agent runs.
func (e *Executor) RespondWithOverrides(step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding) error {
	return e.RespondWithOverrideReason(step, action, findingIDs, instructions, addedFindings, "")
}

// RespondWithOverrideReason allows one attributed review fix round after the
// configured cap has been consumed. The reason is accepted only at that gate.
func (e *Executor) RespondWithOverrideReason(step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding, fixOverrideReason string) error {
	return e.respondWithMetadata(step, action, findingIDs, instructions, addedFindings, fixOverrideReason, false)
}

// RespondWithRetryAttribution preserves whether a retry came from an automatic
// driver so the executor can enforce the one-auto-retry budget durably.
func (e *Executor) RespondWithRetryAttribution(step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding, fixOverrideReason string, autoRetry bool) error {
	return e.respondWithMetadata(step, action, findingIDs, instructions, addedFindings, fixOverrideReason, autoRetry)
}

func (e *Executor) respondWithMetadata(step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding, fixOverrideReason string, autoRetry bool) error {
	fixOverrideReason = strings.TrimSpace(fixOverrideReason)
	e.mu.Lock()
	if !e.waiting {
		e.mu.Unlock()
		return fmt.Errorf("no step awaiting approval")
	}
	if step != e.waitingStep {
		e.mu.Unlock()
		return fmt.Errorf("step mismatch: responding to %q but %q is awaiting approval", step, e.waitingStep)
	}
	if fixOverrideReason != "" && action != types.ActionFix {
		e.mu.Unlock()
		return fmt.Errorf("fix override reason is only valid with --action fix")
	}
	if e.waitingAgentRetry {
		if action != types.ActionRetry {
			e.mu.Unlock()
			return fmt.Errorf("agent transient park for %q requires --action retry", step)
		}
		if len(findingIDs) > 0 || len(instructions) > 0 || len(addedFindings) > 0 {
			e.mu.Unlock()
			return fmt.Errorf("--action retry does not accept findings or fix instructions")
		}
		if autoRetry {
			count, err := e.db.CountStepAgentAutoRetries(e.waitingStepResultID)
			if err != nil {
				e.mu.Unlock()
				return fmt.Errorf("count agent auto retries for %q: %w", step, err)
			}
			if count >= 1 {
				e.mu.Unlock()
				return fmt.Errorf("agent transient auto-retry already consumed for %q; parked for manual retry", step)
			}
		}
	} else if action == types.ActionRetry {
		e.mu.Unlock()
		return fmt.Errorf("--action retry is only valid while a step is awaiting_agent_retry")
	}
	if action == types.ActionFix {
		if e.waitingAtFixRoundCap && fixOverrideReason == "" {
			count, max := e.waitingFixRoundCount, e.waitingMaxFixRounds
			e.mu.Unlock()
			return fmt.Errorf("review max_fix_rounds cap reached (%d consumed, cap %d); step is awaiting_triage for master triage; use --fix-override --override-reason <reason> only after an explicit triage ruling", count, max)
		}
		if !e.waitingAtFixRoundCap && fixOverrideReason != "" {
			e.mu.Unlock()
			return fmt.Errorf("--fix-override is only valid while review is awaiting_triage after max_fix_rounds is reached")
		}
	}
	response := approvalResponse{
		action:            action,
		findingIDs:        findingIDs,
		instructions:      instructions,
		addedFindings:     addedFindings,
		fixOverrideReason: fixOverrideReason,
		autoRetry:         autoRetry,
	}
	select {
	case e.approvalCh <- response:
		e.waiting = false
		e.mu.Unlock()
		return nil
	default:
		e.mu.Unlock()
		return fmt.Errorf("approval response already queued for %q", step)
	}
}

// Execute runs the pipeline steps sequentially for a given run.
// The workDir is the directory where steps execute (typically a git worktree).
// If the context is cancelled with a cause (via context.WithCancelCause),
// the cause message is preserved as the run's error in the DB.
func (e *Executor) Execute(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
	e.workDir = workDir
	ctx = e.runContext(ctx)
	// Mark run as running. Route write failures through failRun so the
	// in-memory lifecycle and subscriber stream still become terminal instead
	// of leaving a silent pending run.
	if err := e.db.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		return e.failRun(run, repo, fmt.Errorf("update run status: %w", err))
	}
	run.Status = types.RunRunning
	e.emitRunEvent(ipc.EventRunUpdated, run, repo)

	// Create log directory for this run
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return e.failRun(run, repo, fmt.Errorf("create log dir: %w", err))
	}

	e.initializeRunScopes(run.ID)

	// Create step result records in DB
	stepRecords := make(map[types.StepName]*db.StepResult)
	for _, step := range e.steps {
		sr, err := e.db.InsertStepResult(run.ID, step.Name())
		if err != nil {
			return e.failRun(run, repo, fmt.Errorf("insert step result: %w", err))
		}
		stepRecords[step.Name()] = sr
	}

	// Execute steps sequentially. A late repair may send the same run back
	// through validation before any new head is published.
	for i := 0; i < len(e.steps); i++ {
		step := e.steps[i]
		if ctx.Err() != nil {
			return e.failRun(run, repo, context.Cause(ctx))
		}

		sr := stepRecords[step.Name()]
		if e.skips[step.Name()] {
			if err := e.db.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
				return e.failRun(run, repo, fmt.Errorf("skip step %s: %w", step.Name(), err), ctx)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, step.Name(), string(types.StepStatusSkipped), "", "", nil)
			continue
		}
		state, err := e.durableExecutionState(sr.ID)
		if err != nil {
			return e.failRun(run, repo, fmt.Errorf("restore step %s execution state: %w", step.Name(), err), ctx)
		}
		skipRemaining, restartFrom, err := e.executeStep(ctx, step, sr, run, repo, workDir, logDir, state)
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if skipRemaining {
			// Mark all subsequent steps as skipped
			for _, remaining := range e.steps[i+1:] {
				rsr := stepRecords[remaining.Name()]
				if dbErr := e.db.CompleteStepWithStatus(rsr.ID, types.StepStatusSkipped, 0, 0, ""); dbErr != nil {
					slog.Warn("failed to finalize skipped step", "step", remaining.Name(), "error", dbErr)
				}
				e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, remaining.Name(), string(types.StepStatusSkipped), "", "", nil)
			}
			break
		}
		if restartFrom != "" {
			restartIndex, err := e.prepareRestart(run.ID, restartFrom, i)
			if err != nil {
				return e.failRun(run, repo, fmt.Errorf("step %s requested invalid restart from %s", step.Name(), restartFrom), ctx)
			}
			i = restartIndex - 1
		}
	}

	// Mark run as completed. A failure here must emit a terminal failure rather
	// than leaving a silent running row after every step has finished.
	if err := e.completeRun(run, repo); err != nil {
		return e.failRun(run, repo, fmt.Errorf("update run status: %w", err))
	}
	return nil
}

func (e *Executor) stepIndex(name types.StepName) (int, error) {
	for index, step := range e.steps {
		if step.Name() == name {
			return index, nil
		}
	}
	return 0, fmt.Errorf("step %s is not in the pipeline", name)
}

func (e *Executor) prepareRestart(runID string, name types.StepName, currentIndex int) (int, error) {
	index, err := e.stepIndex(name)
	if err != nil || index >= currentIndex {
		return 0, fmt.Errorf("invalid restart boundary")
	}
	if err := e.db.ResetStepsFrom(runID, e.steps[index].Name().Order()); err != nil {
		return 0, err
	}
	return index, nil
}

func (e *Executor) initializeRunScopes(runID string) {
	sessionsEnabled := e.config != nil && e.config.SessionReuse && e.agent != nil
	e.sessions = NewRunSessions(e.db, runID, e.agent, sessionsEnabled)
	e.shared = &RunShared{}
}

type stepExecutionState struct {
	fixing           bool
	previousFindings string
	roundNum         int
	autoFixAttempts  int
	executionMS      int64
	currentRoundID   string
}

func (e *Executor) durableExecutionState(stepResultID string) (stepExecutionState, error) {
	rounds, err := e.db.GetRoundsByStep(stepResultID)
	if err != nil {
		return stepExecutionState{}, err
	}
	state := stepExecutionState{}
	for _, round := range rounds {
		state.roundNum = max(state.roundNum, round.Round)
		if round.SelectionSource != nil && *round.SelectionSource == db.RoundSelectionSourceAutoFix {
			state.autoFixAttempts++
		}
	}
	return state, nil
}

type recoveredGate struct {
	index           int
	step            Step
	stepResult      *db.StepResult
	findings        string
	round           int
	autoFixes       int
	lastRoundID     string
	reviewedHeadSHA string
	agentRetry      bool
}

func ValidateRecoveredRun(database *db.DB, run *db.Run, steps []Step) error {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		return fmt.Errorf("run is not a recoverable parked run")
	}
	_, err := (&Executor{db: database, steps: steps}).recoveredGate(run.ID)
	return err
}

// Resume restores a run that was durably parked at an approval gate when the
// daemon stopped. It only accepts a fully recorded gate and otherwise returns
// an error so startup recovery can fail the run rather than guessing.
func (e *Executor) Resume(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
	e.workDir = workDir
	ctx = e.runContext(ctx)
	if repo == nil {
		return fmt.Errorf("recovered run has no repository")
	}
	if err := ValidateRecoveredRun(e.db, run, e.steps); err != nil {
		return err
	}
	gate, err := e.recoveredGate(run.ID)
	if err != nil {
		return err
	}
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return e.failRun(run, repo, fmt.Errorf("create log dir: %w", err))
	}
	e.initializeRunScopes(run.ID)

	parkStart := time.Unix(*run.AwaitingAgentSince, 0)
	duration := recoveredStepDuration(gate.stepResult)
	completeRecoveredGate := func() error {
		if gate.step.Name() == types.StepReview {
			if gate.reviewedHeadSHA == "" {
				return fmt.Errorf("recovered review has no durable reviewed head candidate")
			}
			if err := e.db.CompleteReviewStep(gate.stepResult.ID, run.ID, gate.reviewedHeadSHA, recoveredExitCode(gate.stepResult), duration, recoveredLogPath(gate.stepResult)); err != nil {
				return err
			}
			reviewedHead := gate.reviewedHeadSHA
			run.ReviewApprovedHeadSHA = &reviewedHead
			ClearUncertifiedPipelineRangeIfCertified(ctx, e.db, repo.ID, run.Branch, reviewedHead, workDir)
			return nil
		}
		return e.db.CompleteStepWithStatus(gate.stepResult.ID, types.StepStatusCompleted, recoveredExitCode(gate.stepResult), duration, recoveredLogPath(gate.stepResult))
	}
	completeReconciledGate := func() error {
		if err := completeRecoveredGate(); err != nil {
			return e.failRun(run, repo, fmt.Errorf("complete reconciled step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusCompleted), "", "", &duration)
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	}
	reconcileCtx := &StepContext{
		Ctx:          ctx,
		Run:          run,
		Repo:         repo,
		WorkDir:      workDir,
		Config:       e.config,
		ForgeContext: e.forge,
		DB:           e.db,
		Agent:        e.agent,
		Sessions:     e.sessions,
		Shared:       e.shared,
		Log: func(message string) {
			slog.Info("recovered approval gate reconciliation", "run_id", run.ID, "step", gate.step.Name(), "message", message)
		},
		LogChunk:   func(string) {},
		LogFile:    func(string) {},
		OnPRMerged: e.onPRMerged,
	}
	waitStep := gate.step
	if gate.agentRetry {
		waitStep = approvalOnlyStep{Step: gate.step}
	}
	if reconciled, reconcileErr := e.reconcileApprovalGate(ctx, waitStep, reconcileCtx); reconciled {
		parkedMS := time.Since(parkStart).Milliseconds()
		if dbErr := e.db.ExitReconciledApprovalGate(context.Background(), run.ID, gate.stepResult.ID, types.StepStatusCompleted, parkedMS, nil); dbErr != nil {
			exitErr := e.recoverApprovalGateExit(run.ID, gate.stepResult.ID, parkedMS, fmt.Errorf("exit reconciled approval gate for step %s: %w", gate.step.Name(), dbErr))
			return e.failRun(run, repo, exitErr, ctx)
		}
		return completeReconciledGate()
	} else if reconcileErr != nil && ctx.Err() == nil {
		if errors.Is(reconcileErr, ErrFatalGateReconciliation) {
			parkedMS := time.Since(parkStart).Milliseconds()
			reason := reconcileErr.Error()
			if dbErr := e.db.ExitApprovalGate(context.Background(), run.ID, gate.stepResult.ID, types.StepStatusFailed, parkedMS, &reason); dbErr != nil {
				exitErr := e.recoverApprovalGateExit(run.ID, gate.stepResult.ID, parkedMS, fmt.Errorf("exit fatally unreconciled approval gate for step %s: %w", gate.step.Name(), dbErr))
				return e.failRun(run, repo, exitErr, ctx)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", reconcileErr.Error(), &duration)
			return e.failRun(run, repo, fmt.Errorf("step %s: reconcile approval gate: %w", gate.step.Name(), reconcileErr), ctx)
		}
		slog.Warn("could not reconcile recovered approval gate; preserving it", "run_id", run.ID, "step", gate.step.Name(), "error", reconcileErr)
	}

	e.mu.Lock()
	e.waiting = true
	e.waitingStep = gate.step.Name()
	e.waitingStepResultID = gate.stepResult.ID
	e.waitingAgentRetry = gate.agentRetry
	if gate.stepResult.Status == types.StepStatusAwaitingTriage {
		count, countErr := e.db.CountStepFixRounds(gate.stepResult.ID)
		if countErr != nil {
			e.mu.Unlock()
			return e.failRun(run, repo, fmt.Errorf("count recovered review fix rounds: %w", countErr), ctx)
		}
		e.waitingAtFixRoundCap = true
		e.waitingFixRoundCount = count
		if e.config != nil {
			e.waitingMaxFixRounds = e.config.Review.MaxFixRounds
		}
	}
	e.mu.Unlock()
	e.emitStepEventWithFindingsAndError(
		ipc.EventStepCompleted,
		run,
		repo,
		gate.step.Name(),
		string(gate.stepResult.Status),
		gate.findings,
		recoveredGateError(gate.stepResult),
		gate.stepResult.DurationMS,
	)

	response, reconciled, err := e.waitForApprovalOrReconcile(ctx, waitStep, reconcileCtx, false)
	parkedMS := time.Since(parkStart).Milliseconds()
	if err == nil && !reconciled && response.action == types.ActionFix && response.fixOverrideReason != "" {
		var persistErr error
		if gate.lastRoundID == "" {
			persistErr = fmt.Errorf("step %s: cannot persist fix override reason (no round record); refusing unattributed override fix round", gate.step.Name())
		} else if dbErr := e.db.SetStepRoundFixOverrideReason(gate.lastRoundID, response.fixOverrideReason); dbErr != nil {
			persistErr = fmt.Errorf("step %s: persist fix override reason: %w", gate.step.Name(), dbErr)
		}
		if persistErr != nil {
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", persistErr.Error(), &duration)
			return e.failRun(run, repo, e.failApprovalGateBeforeExit(run.ID, gate.stepResult.ID, parkedMS, persistErr), ctx)
		}
	}
	exitStatus, exitReason := approvalExitState(response, err)
	if reconciled {
		exitStatus, exitReason = types.StepStatusCompleted, nil
	}
	exitGate := e.db.ExitApprovalGate
	if reconciled {
		exitGate = e.db.ExitReconciledApprovalGate
	}
	if dbErr := exitGate(context.Background(), run.ID, gate.stepResult.ID, exitStatus, parkedMS, exitReason); dbErr != nil {
		exitErr := e.recoverApprovalGateExit(run.ID, gate.stepResult.ID, parkedMS, fmt.Errorf("exit recovered approval gate for step %s: %w", gate.step.Name(), dbErr))
		return e.failRun(run, repo, exitErr, ctx)
	}
	if err != nil {
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", err.Error(), &duration)
		return e.failRun(run, repo, fmt.Errorf("step %s: waiting for approval: %w", gate.step.Name(), err), ctx)
	}
	if reconciled {
		return completeReconciledGate()
	}

	approvalFields := telemetry.Fields{
		"step":       string(gate.step.Name()),
		"action":     string(response.action),
		"fix_review": gate.stepResult.Status == types.StepStatusFixReview,
	}
	if agentName := e.telemetryAgentName(); agentName != "" {
		approvalFields["agent"] = agentName
	}
	if selectedCount := selectedFindingCount(gate.findings, response.findingIDs); selectedCount > 0 {
		approvalFields["selected_findings_count"] = selectedCount
	}
	if response.fixOverrideReason != "" {
		approvalFields["fix_override"] = true
	}
	telemetry.Track("approval", approvalFields)
	switch response.action {
	case types.ActionApprove:
		e.recordDeclinedRound(gate.lastRoundID, gate.findings, gate.step.Name(), gate.round)
		if err := completeRecoveredGate(); err != nil {
			return e.failRun(run, repo, fmt.Errorf("complete recovered step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusCompleted), "", "", &duration)
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	case types.ActionSkip:
		e.recordDeclinedRound(gate.lastRoundID, gate.findings, gate.step.Name(), gate.round)
		if err := e.db.CompleteStepWithStatus(gate.stepResult.ID, types.StepStatusSkipped, recoveredExitCode(gate.stepResult), duration, recoveredLogPath(gate.stepResult)); err != nil {
			return e.failRun(run, repo, fmt.Errorf("skip recovered step %s: %w", gate.step.Name(), err), ctx)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusSkipped), "", "", &duration)
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	case types.ActionAbort:
		e.recordDeclinedRound(gate.lastRoundID, gate.findings, gate.step.Name(), gate.round)
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFailed), "", "aborted by user", &duration)
		return e.failRun(run, repo, fmt.Errorf("step %s: aborted by user", gate.step.Name()), ctx)
	case types.ActionFix:
		telemetry.Track("fix", e.fixTelemetryFields("user", gate.step.Name(), selectedFindingCount(gate.findings, response.findingIDs), 0))
		selected := filterFindingsJSON(gate.findings, response.findingIDs)
		merged := mergeUserOverridesJSON(selected, response.instructions, response.addedFindings)
		if gate.lastRoundID != "" {
			allSelectedIDs := combineSelectedFindingIDs(response.findingIDs, merged)
			if idsJSON := marshalFindingIDs(allSelectedIDs); idsJSON != "" {
				var userFindingsJSON *string
				if merged != "" && merged != selected {
					userFindingsJSON = &merged
				}
				selectionSource := db.RoundSelectionSourceUser
				if response.fixOverrideReason != "" {
					selectionSource = db.RoundSelectionSourceUserOverride
				}
				if dbErr := e.db.SetStepRoundUserDecision(gate.lastRoundID, &idsJSON, selectionSource, userFindingsJSON); dbErr != nil {
					slog.Warn("failed to record recovered user decision", "step", gate.step.Name(), "round", gate.round, "error", dbErr)
				}
			}
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, gate.step.Name(), string(types.StepStatusFixing), "", "", nil)
		skipRemaining, restartFrom, err := e.executeStep(ctx, gate.step, gate.stepResult, run, repo, workDir, logDir, stepExecutionState{
			fixing:           true,
			previousFindings: merged,
			roundNum:         gate.round,
			autoFixAttempts:  gate.autoFixes,
			executionMS:      duration,
			currentRoundID:   gate.lastRoundID,
		})
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if skipRemaining {
			return e.skipRecoveredRemainder(run, repo, gate.index+1)
		}
		if restartFrom != "" {
			restartIndex, indexErr := e.prepareRestart(run.ID, restartFrom, gate.index)
			if indexErr != nil {
				return e.failRun(run, repo, fmt.Errorf("step %s requested invalid restart from %s", gate.step.Name(), restartFrom), ctx)
			}
			return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, restartIndex, true)
		}
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	case types.ActionRetry:
		if !gate.agentRetry {
			return e.failRun(run, repo, fmt.Errorf("step %s: retry action outside agent retry gate", gate.step.Name()), ctx)
		}
		trigger := db.RoundTriggerAgentManualRetry
		if response.autoRetry {
			trigger = db.RoundTriggerAgentAutoRetry
		}
		if _, dbErr := e.db.InsertStepRound(gate.stepResult.ID, gate.round+1, trigger, nil, nil, 0); dbErr != nil {
			return e.failRun(run, repo, fmt.Errorf("step %s: persist recovered agent retry attribution: %w", gate.step.Name(), dbErr), ctx)
		}
		telemetry.Track("agent_retry", telemetry.Fields{"step": string(gate.step.Name()), "auto": response.autoRetry, "recovered": true})
		e.emitStepEvent(ipc.EventStepStarted, run, repo, gate.step.Name(), string(types.StepStatusRunning))
		state, stateErr := e.durableExecutionState(gate.stepResult.ID)
		if stateErr != nil {
			return e.failRun(run, repo, fmt.Errorf("restore retried step %s execution state: %w", gate.step.Name(), stateErr), ctx)
		}
		state.executionMS = duration
		skipRemaining, restartFrom, executeErr := e.executeStep(ctx, gate.step, gate.stepResult, run, repo, workDir, logDir, state)
		if executeErr != nil {
			return e.failRun(run, repo, executeErr, ctx)
		}
		if skipRemaining {
			return e.skipRecoveredRemainder(run, repo, gate.index+1)
		}
		if restartFrom != "" {
			restartIndex, indexErr := e.prepareRestart(run.ID, restartFrom, gate.index)
			if indexErr != nil {
				return e.failRun(run, repo, fmt.Errorf("step %s requested invalid restart from %s", gate.step.Name(), restartFrom), ctx)
			}
			return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, restartIndex, true)
		}
		return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, gate.index+1, false)
	default:
		return e.failRun(run, repo, fmt.Errorf("step %s: unsupported approval action %q", gate.step.Name(), response.action), ctx)
	}
}

func (e *Executor) runContext(ctx context.Context) context.Context {
	if e.forge == nil {
		return ctx
	}
	return git.WithEnvironment(ctx, e.forge.Environment)
}

func (e *Executor) recoveredGate(runID string) (*recoveredGate, error) {
	results, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("get recovered steps: %w", err)
	}
	if len(results) != len(e.steps) {
		return nil, fmt.Errorf("recovered run has %d step records for %d steps", len(results), len(e.steps))
	}

	var gate *recoveredGate
	for index, result := range results {
		if result.StepName != e.steps[index].Name() {
			return nil, fmt.Errorf("recovered step %d is %q, want %q", index, result.StepName, e.steps[index].Name())
		}
		if result.Status == types.StepStatusAwaitingRetry {
			if gate != nil || result.StartedAt == nil || result.DurationMS == nil || result.AgentPID != nil || result.Error == nil || strings.TrimSpace(*result.Error) == "" {
				return nil, fmt.Errorf("recovered agent retry gate is incomplete")
			}
			state, stateErr := e.durableExecutionState(result.ID)
			if stateErr != nil {
				return nil, fmt.Errorf("restore recovered agent retry state: %w", stateErr)
			}
			gate = &recoveredGate{
				index:      index,
				step:       e.steps[index],
				stepResult: result,
				round:      state.roundNum,
				autoFixes:  state.autoFixAttempts,
				agentRetry: true,
			}
			continue
		}
		if result.Status == types.StepStatusAwaitingApproval || result.Status == types.StepStatusFixReview || result.Status == types.StepStatusAwaitingTriage {
			if gate != nil || result.FindingsJSON == nil || result.StartedAt == nil || result.DurationMS == nil || result.AgentPID != nil {
				return nil, fmt.Errorf("recovered approval gate is incomplete")
			}
			rounds, err := e.db.GetRoundsByStep(result.ID)
			if err != nil || len(rounds) == 0 {
				return nil, fmt.Errorf("recovered approval gate has no complete round")
			}
			latest := rounds[len(rounds)-1]
			if latest.FindingsJSON == nil || *latest.FindingsJSON != *result.FindingsJSON {
				return nil, fmt.Errorf("recovered approval gate findings are incomplete")
			}
			autoFixes := 0
			for _, round := range rounds {
				if round.SelectionSource != nil && *round.SelectionSource == db.RoundSelectionSourceAutoFix {
					autoFixes++
				}
			}
			gate = &recoveredGate{
				index:       index,
				step:        e.steps[index],
				stepResult:  result,
				findings:    *result.FindingsJSON,
				round:       latest.Round,
				autoFixes:   autoFixes,
				lastRoundID: latest.ID,
			}
			if latest.ReviewedHeadSHA != nil {
				gate.reviewedHeadSHA = *latest.ReviewedHeadSHA
			}
			continue
		}
		if gate == nil {
			if result.Status != types.StepStatusCompleted && result.Status != types.StepStatusSkipped {
				return nil, fmt.Errorf("recovered step %s is %s before approval gate", result.StepName, result.Status)
			}
			continue
		}
		if result.Status != types.StepStatusPending && result.Status != types.StepStatusSkipped {
			return nil, fmt.Errorf("recovered step %s is %s after approval gate", result.StepName, result.Status)
		}
	}
	if gate == nil {
		return nil, fmt.Errorf("recovered run has no approval gate")
	}
	return gate, nil
}

func (e *Executor) executeRecoveredRemainder(ctx context.Context, run *db.Run, repo *db.Repo, workDir, logDir string, start int, revalidating bool) error {
	results, err := e.db.GetStepsByRun(run.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("get recovered steps: %w", err), ctx)
	}
	for index := start; index < len(e.steps); index++ {
		if ctx.Err() != nil {
			return e.failRun(run, repo, context.Cause(ctx), ctx)
		}
		if index >= len(results) || results[index].StepName != e.steps[index].Name() || (!revalidating && results[index].Status != types.StepStatusPending && results[index].Status != types.StepStatusSkipped) {
			return e.failRun(run, repo, fmt.Errorf("recovered step plan changed at %d", index), ctx)
		}
		if results[index].Status == types.StepStatusSkipped {
			continue
		}
		state, stateErr := e.durableExecutionState(results[index].ID)
		if stateErr != nil {
			return e.failRun(run, repo, fmt.Errorf("restore step %s execution state: %w", e.steps[index].Name(), stateErr), ctx)
		}
		skipRemaining, restartFrom, err := e.executeStep(ctx, e.steps[index], results[index], run, repo, workDir, logDir, state)
		if err != nil {
			return e.failRun(run, repo, err, ctx)
		}
		if skipRemaining {
			return e.skipRecoveredRemainder(run, repo, index+1)
		}
		if restartFrom != "" {
			restartIndex, indexErr := e.prepareRestart(run.ID, restartFrom, index)
			if indexErr != nil {
				return e.failRun(run, repo, fmt.Errorf("step %s requested invalid restart from %s", e.steps[index].Name(), restartFrom), ctx)
			}
			revalidating = true
			index = restartIndex - 1
		}
	}
	if err := e.completeRun(run, repo); err != nil {
		return e.failRun(run, repo, fmt.Errorf("complete recovered run: %w", err), ctx)
	}
	return nil
}

func (e *Executor) skipRecoveredRemainder(run *db.Run, repo *db.Repo, start int) error {
	results, err := e.db.GetStepsByRun(run.ID)
	if err != nil {
		return e.failRun(run, repo, fmt.Errorf("get recovered steps: %w", err))
	}
	for index := start; index < len(e.steps); index++ {
		if index >= len(results) || results[index].StepName != e.steps[index].Name() || results[index].Status != types.StepStatusPending {
			return e.failRun(run, repo, fmt.Errorf("recovered step plan changed at %d", index))
		}
		if err := e.db.CompleteStepWithStatus(results[index].ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
			return e.failRun(run, repo, fmt.Errorf("skip recovered step %s: %w", e.steps[index].Name(), err))
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, e.steps[index].Name(), string(types.StepStatusSkipped), "", "", nil)
	}
	if err := e.completeRun(run, repo); err != nil {
		return e.failRun(run, repo, fmt.Errorf("complete recovered run: %w", err))
	}
	return nil
}

func recoveredStepDuration(step *db.StepResult) int64 {
	if step != nil && step.DurationMS != nil {
		return *step.DurationMS
	}
	return 0
}

func recoveredGateError(step *db.StepResult) string {
	if step == nil || step.Error == nil {
		return ""
	}
	return *step.Error
}

func recoveredExitCode(step *db.StepResult) int {
	if step != nil && step.ExitCode != nil {
		return *step.ExitCode
	}
	return 0
}

func recoveredLogPath(step *db.StepResult) string {
	if step != nil && step.LogPath != nil {
		return *step.LogPath
	}
	return ""
}

// executeStep runs a single step with approval coordination.
// Returns whether to skip the remainder, an optional earlier restart step,
// and any execution error.
func (e *Executor) executeStep(ctx context.Context, step Step, sr *db.StepResult, run *db.Run, repo *db.Repo, workDir, logDir string, state stepExecutionState) (bool, types.StepName, error) {
	stepName := step.Name()
	logPath := filepath.Join(logDir, string(stepName)+".log")
	finalExitCode := 0
	autoFixLimit := 0
	if e.config != nil {
		autoFixLimit = e.config.AutoFixLimit(stepName)
	}

	// Mark step as running
	if err := e.db.StartStepWithAutoFixLimit(sr.ID, autoFixLimit); err != nil {
		return false, "", fmt.Errorf("start step %s: %w", stepName, err)
	}
	e.emitStepEvent(ipc.EventStepStarted, run, repo, stepName, string(types.StepStatusRunning))

	// Track execution-only time, excluding approval wait periods.
	phaseStart := time.Now()
	executionMS := state.executionMS
	var durationOverrideMS int64 // sum of step-reported overrides (demo mode)

	// Open log file for persistent step logging
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, "", fmt.Errorf("create step log file %s: %w", stepName, err)
	}
	defer logFile.Close()

	// Build step context with log callback that emits events and writes to file.
	// lastChunkNewline tracks whether the most recent chunk ended with \n,
	// so Log knows whether it needs a leading \n to flush a streaming partial.
	lastChunkNewline := true
	userIntent := ""
	userIntentSource := ""
	if run != nil {
		if run.Intent != nil {
			userIntent = *run.Intent
		}
		// Propagate provenance alongside the text so steps can distinguish an
		// explicit, authoritative `--intent` (Source=="agent") from a
		// transcript-inferred hint. Dropping this is the provenance-erasure
		// bug that let an authoritative intent be demoted to an ignorable hint.
		if run.IntentSource != nil {
			userIntentSource = *run.IntentSource
		}
	}
	lastLogActivityAt := time.Time{}
	touchLogActivity := func(text string, force bool) {
		if activity := stepActivityFromLog(text); activity != "" {
			now := time.Now()
			if !force && !lastLogActivityAt.IsZero() && now.Sub(lastLogActivityAt) < stepActivityThrottleInterval {
				return
			}
			lastLogActivityAt = now
			if dbErr := e.db.TouchStepActivity(sr.ID, activity); dbErr != nil {
				slog.Warn("failed to touch step activity in db", "step", stepName, "error", dbErr)
			}
		}
	}
	writeLog := func(text string) {
		if text != "" {
			prefix := ""
			if !lastChunkNewline {
				prefix = "\n"
			}
			text = prefix + strings.TrimRight(text, "\n") + "\n\n"
			lastChunkNewline = true
		}
		e.emitLogChunk(run, repo, stepName, text)
		fmt.Fprint(logFile, text)
		touchLogActivity(text, true)
	}
	writeLogChunk := func(text string) {
		if text != "" {
			lastChunkNewline = strings.HasSuffix(text, "\n")
		}
		e.emitLogChunk(run, repo, stepName, text)
		fmt.Fprint(logFile, text)
		touchLogActivity(text, strings.Contains(text, "\n"))
	}
	onAgentLifecycle := func(event agent.LifecycleEvent) {
		text := event.Message
		if text == "" {
			text = fmt.Sprintf("%s %s", event.Agent, event.Phase)
		}
		switch event.Phase {
		case agent.LifecyclePhaseStart:
			pid := event.PID
			if dbErr := e.db.SetStepAgentActivity(sr.ID, text, &pid); dbErr != nil {
				slog.Warn("failed to set step agent activity in db", "step", stepName, "error", dbErr)
			}
		case agent.LifecyclePhaseExit:
			if dbErr := e.db.SetStepAgentActivity(sr.ID, text, nil); dbErr != nil {
				slog.Warn("failed to set step agent activity in db", "step", stepName, "error", dbErr)
			}
		default:
			if dbErr := e.db.TouchStepActivity(sr.ID, text); dbErr != nil {
				slog.Warn("failed to touch step activity in db", "step", stepName, "error", dbErr)
			}
		}
		writeLog(text)
	}
	// roundNum is shared with the perf wrapper's round closure below: an
	// invocation during execution of round N+1 sees roundNum still at N.
	autoFixAttempts := state.autoFixAttempts
	roundNum := state.roundNum

	stepAgent := e.agent
	if stepAgent != nil {
		// Innermost: default-by-construction invocation deadline so a step
		// that calls Agent.Run directly cannot hang the run.
		stepAgent = &timeoutAgent{inner: stepAgent, timeout: AgentTimeout(e.config)}
		stepAgent = &gateStepBoundaryAgent{inner: stepAgent, phase: stepName}
		stepAgent = &lifecycleAgent{inner: stepAgent, onLifecycle: onAgentLifecycle}
		stepAgent = &perfRecordingAgent{
			inner:    stepAgent,
			db:       e.db,
			runID:    run.ID,
			stepName: stepName,
			round:    func() int { return roundNum + 1 },
		}
	}
	ciReady := run.CIReadyAt != nil
	ciReadyNoCI := run.CIReadyNoCI
	ciReadinessChanged := func(ready, declaredNoCI bool) {
		declaredNoCI = ready && declaredNoCI
		if ciReady == ready && ciReadyNoCI == declaredNoCI {
			return
		}
		ciReady = ready
		ciReadyNoCI = declaredNoCI
		e.emitCIReadinessEvent(run, repo, ready, declaredNoCI)
	}
	designContext := types.DesignContext{}
	if run != nil && run.DesignContextJSON != nil {
		parsed, err := types.ParseDesignContextJSON(*run.DesignContextJSON)
		if err != nil {
			return false, "", fmt.Errorf("parse run design context: %w", err)
		}
		designContext = parsed
	}
	sctx := &StepContext{
		Ctx:              ctx,
		Run:              run,
		Repo:             repo,
		WorkDir:          workDir,
		Agent:            stepAgent,
		Config:           e.config,
		ForgeContext:     e.forge,
		DB:               e.db,
		StepResultID:     sr.ID,
		UserIntent:       userIntent,
		DesignContext:    designContext,
		IntentSource:     userIntentSource,
		Sessions:         e.sessions,
		Shared:           e.shared,
		EvidenceDir:      e.runEvidenceDir(run.ID),
		Fixing:           state.fixing,
		PreviousFindings: state.previousFindings,
		Log:              writeLog,
		LogChunk:         writeLogChunk,
		LogFile: func(text string) {
			fmt.Fprintln(logFile, text)
			touchLogActivity(text, true)
		},
		CIReadinessChanged: ciReadinessChanged,
		OnPRMerged:         e.onPRMerged,
	}
	if stepName == types.StepReview {
		BindUncertifiedPipelineRange(sctx)
	}
	// Every step, not just review: the steps that used to re-apply a declined
	// change were precisely the ones a decision never reached.
	BindBranchDecisions(sctx)

	nextTrigger := "initial"
	if sctx.Fixing {
		nextTrigger = "auto_fix"
	}
	skipRemaining := false
	stepSkipped := false
	currentRoundID := state.currentRoundID
	var reviewApprovedHeadSHA string
	var restartFrom types.StepName

	// Execute with possible fix loop
	for {
		reviewStartingHeadSHA := run.HeadSHA
		sctx.ReviewStartingHeadSHA = reviewStartingHeadSHA
		outcome, err := step.Execute(sctx)
		roundNum++
		roundDuration := time.Since(phaseStart).Milliseconds()
		if err != nil {
			durationMS := executionMS + roundDuration
			var transient *agent.TransientError
			if errors.As(err, &transient) {
				executionMS = durationMS
				reason := safeurl.RedactText(agentTransientParkReason(transient))
				fmt.Fprintf(logFile, "\n%s\n", reason)
				touchLogActivity(reason, true)
				parkStart := time.Now()

				e.mu.Lock()
				e.waiting = true
				e.waitingStep = stepName
				e.waitingStepResultID = sr.ID
				e.waitingAgentRetry = true
				e.waitingAtFixRoundCap = false
				e.waitingFixRoundCount = 0
				e.waitingMaxFixRounds = 0
				e.mu.Unlock()

				if _, dbErr := e.db.EnterApprovalGate(ctx, run.ID, sr.ID, types.StepStatusAwaitingRetry, executionMS, &reason); dbErr != nil {
					e.clearApprovalWaitState()
					publishErr := fmt.Errorf("publish agent retry gate for step %s: %w", stepName, dbErr)
					return false, "", e.failGatePublication(stepName, sr.ID, executionMS, publishErr)
				}
				e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusAwaitingRetry), "", reason, &executionMS)

				response, _, waitErr := e.waitForApprovalOrReconcile(ctx, approvalOnlyStep{Step: step}, sctx, false)
				exitStatus := types.StepStatusRunning
				var exitReason *string
				if waitErr != nil {
					exitStatus = types.StepStatusFailed
					reason := waitErr.Error()
					exitReason = &reason
				} else if response.action != types.ActionRetry {
					exitStatus = types.StepStatusFailed
					reason := fmt.Sprintf("agent transient park requires retry action, got %s", response.action)
					exitReason = &reason
				}
				parkedMS := time.Since(parkStart).Milliseconds()
				if dbErr := e.db.ExitApprovalGate(context.Background(), run.ID, sr.ID, exitStatus, parkedMS, exitReason); dbErr != nil {
					exitErr := e.recoverApprovalGateExit(run.ID, sr.ID, parkedMS, fmt.Errorf("exit agent retry gate for step %s: %w", stepName, dbErr))
					return false, "", exitErr
				}
				if waitErr != nil {
					e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", waitErr.Error(), &executionMS)
					return false, "", fmt.Errorf("step %s: waiting for agent retry: %w", stepName, waitErr)
				}
				if response.action != types.ActionRetry {
					return false, "", fmt.Errorf("step %s: agent transient park requires retry action, got %s", stepName, response.action)
				}
				trigger := db.RoundTriggerAgentManualRetry
				if response.autoRetry {
					trigger = db.RoundTriggerAgentAutoRetry
				}
				if _, dbErr := e.db.InsertStepRound(sr.ID, roundNum, trigger, nil, nil, 0); dbErr != nil {
					persistErr := fmt.Errorf("step %s: persist agent retry attribution: %w", stepName, dbErr)
					_ = e.db.FailStep(sr.ID, persistErr.Error(), executionMS)
					return false, "", persistErr
				}
				telemetry.Track("agent_retry", telemetry.Fields{"step": string(stepName), "auto": response.autoRetry, "agent": transient.Agent, "label": transient.Label})
				phaseStart = time.Now()
				e.emitStepEvent(ipc.EventStepStarted, run, repo, stepName, string(types.StepStatusRunning))
				continue
			}
			// Persist the failure reason to the step's own log file. The error
			// often carries the only detail of why the step failed (e.g. git
			// stderr from a rejected push); without this the step log shows the
			// work starting but never why it stopped. Redact defensively so a
			// credentialled upstream URL that slipped into a wrapped error can
			// never land in the log file.
			redactedErr := safeurl.RedactText(err.Error())
			fmt.Fprintf(logFile, "\nerror: %s\n", redactedErr)
			touchLogActivity("error: "+redactedErr, true)
			if dbErr := e.db.FailStep(sr.ID, redactedErr, durationMS); dbErr != nil {
				slog.Warn("failed to mark step as failed in db", "step", stepName, "error", dbErr)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", redactedErr, &durationMS)
			return false, "", fmt.Errorf("step %s failed: %s", stepName, redactedErr)
		}
		restartFrom = outcome.RestartFrom

		if stepName == types.StepReview {
			reviewApprovedHeadSHA = outcome.ReviewApprovedHeadSHA
		}
		outcome.Findings = normalizeFindingsJSON(outcome.Findings, string(stepName))
		finalExitCode = outcome.ExitCode
		durationOverrideMS += outcome.DurationOverrideMS

		var findingsPersistErr error
		if outcome.Findings != "" {
			if dbErr := e.db.SetStepFindings(sr.ID, outcome.Findings); dbErr != nil {
				findingsPersistErr = dbErr
				slog.Warn("failed to set step findings in db", "step", stepName, "error", dbErr)
			}
		} else {
			if dbErr := e.db.ClearStepFindings(sr.ID); dbErr != nil {
				findingsPersistErr = dbErr
				slog.Warn("failed to clear step findings in db", "step", stepName, "error", dbErr)
			}
		}

		// Persist this execution round.
		var findingsPtr *string
		if outcome.Findings != "" {
			findingsPtr = &outcome.Findings
		}
		var fixSummaryPtr *string
		if outcome.FixSummary != "" {
			s := outcome.FixSummary
			fixSummaryPtr = &s
		}
		var inserted *db.StepRound
		var dbErr error
		roundTrigger := nextTrigger
		if stepName == types.StepCI && restartFrom != "" && !sctx.Fixing {
			roundTrigger = "auto_fix"
		}
		if stepName == types.StepReview {
			if e.config != nil && e.config.CaptureEvalProvenance {
				inserted, dbErr = e.db.InsertReviewStepRoundWithProvenance(sr.ID, roundNum, roundTrigger, findingsPtr, fixSummaryPtr, reviewApprovedHeadSHA, reviewStartingHeadSHA, e.config.TrustedConfigSHA, e.config.ReplayGlobalYAML, e.config.ReplayRepoYAML, roundDuration)
			} else {
				inserted, dbErr = e.db.InsertReviewStepRound(sr.ID, roundNum, roundTrigger, findingsPtr, fixSummaryPtr, reviewApprovedHeadSHA, roundDuration)
			}
		} else {
			inserted, dbErr = e.db.InsertStepRound(sr.ID, roundNum, roundTrigger, findingsPtr, fixSummaryPtr, roundDuration)
		}
		if dbErr != nil {
			currentRoundID = roundInsertID(currentRoundID, inserted, dbErr)
			slog.Warn("failed to insert step round", "step", stepName, "round", roundNum, "error", dbErr)
		} else {
			currentRoundID = roundInsertID(currentRoundID, inserted, nil)
		}

		// If the step produced a PR URL, propagate it to the run and emit an update.
		if outcome.PRURL != "" {
			run.PRURL = &outcome.PRURL
			e.emitRunEvent(ipc.EventRunUpdated, run, repo)
		}

		reviewCapReached, reviewFixRoundCount, reviewMaxFixRounds, capErr := e.reviewFixRoundCapReached(stepName, sr.ID)
		if capErr != nil {
			return false, "", fmt.Errorf("step %s: enforce review max_fix_rounds: %w", stepName, capErr)
		}

		// Check if auto-fix should be attempted.
		// Only auto-fix findings whose action is "auto-fix".
		// This runs before the NeedsApproval check so that all severity
		// levels (including "info") get a chance at automatic fixing.
		if outcome.AutoFixable && autoFixLimit > 0 && autoFixAttempts < autoFixLimit {
			fixableFindings := autoFixableFindingsJSON(outcome.Findings)
			if fixableFindings != "" && !reviewCapReached {
				autoFixAttempts++
				telemetry.Track("fix", e.fixTelemetryFields("auto", stepName, findingsCount(fixableFindings), autoFixAttempts))
				slog.Info("auto-fixing step", "step", stepName, "attempt", autoFixAttempts, "max", autoFixLimit)
				executionMS += time.Since(phaseStart).Milliseconds()
				fixCount := findingsCount(fixableFindings)
				writeLog(fmt.Sprintf("auto-fix round %d/%d starting after round %d (%d %s)", autoFixAttempts, autoFixLimit, roundNum, fixCount, pluralize(fixCount, "finding", "findings")))
				if dbErr := e.db.UpdateStepStatus(sr.ID, types.StepStatusFixing); dbErr != nil {
					slog.Warn("failed to update step status in db", "step", stepName, "status", "fixing", "error", dbErr)
				}
				if currentRoundID != "" {
					if idsJSON := findingIDsJSON(fixableFindings); idsJSON != "" {
						if dbErr := e.db.SetStepRoundSelection(currentRoundID, &idsJSON, db.RoundSelectionSourceAutoFix); dbErr != nil {
							slog.Warn("failed to record selected finding ids", "step", stepName, "round", roundNum, "error", dbErr)
						}
					}
				}
				e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFixing), "", "", nil)
				phaseStart = time.Now()
				sctx.Fixing = true
				sctx.PreviousFindings = fixableFindings
				nextTrigger = "auto_fix"
				continue
			} else if fixableFindings != "" && reviewCapReached {
				slog.Info("review fix-round cap reached; parking for triage", "step", stepName, "fix_rounds", reviewFixRoundCount, "max_fix_rounds", reviewMaxFixRounds)
			}
		}

		if !outcome.NeedsApproval && !hasAskUserFindingsJSON(outcome.Findings) {
			// Step completed without needing approval.
			// Any remaining info-only or non-blocking findings
			// are acceptable and don't block the pipeline.
			skipRemaining = outcome.SkipRemaining
			stepSkipped = outcome.Skipped
			break
		}

		// Freeze execution timer before entering approval wait.
		executionMS += time.Since(phaseStart).Milliseconds()

		// Determine approval status: fix_review after a fix cycle, awaiting_approval otherwise.
		// The working-tree diff that shows what the agent changed is NOT
		// attached here: it is unbounded, and one frame over the transport
		// limit kills the whole subscription and hides every event after it.
		// Consumers fetch it on demand from the run's worktree instead
		// (ipc.MethodGetStepDiff).
		approvalStatus := types.StepStatusAwaitingApproval
		if reviewCapReached {
			approvalStatus = types.StepStatusAwaitingTriage
		} else if sctx.Fixing {
			approvalStatus = types.StepStatusFixReview
		}
		if findingsPersistErr != nil {
			persistErr := fmt.Errorf("persist %s approval gate: %w", stepName, findingsPersistErr)
			return false, "", e.failGatePublication(stepName, sr.ID, executionMS, persistErr)
		}

		// Mark executor as ready to receive approval before updating DB or
		// emitting events, so that callers who poll the DB status can
		// immediately call Respond once they see it.
		e.mu.Lock()
		e.waiting = true
		e.waitingStep = stepName
		e.waitingStepResultID = sr.ID
		e.waitingAgentRetry = false
		e.waitingAtFixRoundCap = reviewCapReached
		e.waitingFixRoundCount = reviewFixRoundCount
		e.waitingMaxFixRounds = reviewMaxFixRounds
		e.mu.Unlock()

		// Parking starts before the gate becomes observable. This includes the
		// small handoff from publishing the gate to receiving a response, and
		// prevents a prompt response from being omitted from the parked total.
		parkStart := time.Now()

		// Publish the run marker and step gate in one transaction. If either
		// write fails, clear the in-memory waiter and fail instead of blocking at
		// a gate that status readers cannot observe.
		if _, dbErr := e.db.EnterApprovalGate(ctx, run.ID, sr.ID, approvalStatus, executionMS, nil); dbErr != nil {
			e.clearApprovalWaitState()
			publishErr := fmt.Errorf("publish approval gate for step %s: %w", stepName, dbErr)
			return false, "", e.failGatePublication(stepName, sr.ID, executionMS, publishErr)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(approvalStatus), outcome.Findings, "", &executionMS)

		response, reconciled, err := e.waitForApprovalOrReconcile(ctx, step, sctx, true)
		parkedMS := time.Since(parkStart).Milliseconds()
		if err == nil && !reconciled && response.action == types.ActionFix && response.fixOverrideReason != "" {
			var persistErr error
			if currentRoundID == "" {
				persistErr = fmt.Errorf("step %s: cannot persist fix override reason (no round record); refusing unattributed override fix round", stepName)
			} else if dbErr := e.db.SetStepRoundFixOverrideReason(currentRoundID, response.fixOverrideReason); dbErr != nil {
				persistErr = fmt.Errorf("step %s: persist fix override reason: %w", stepName, dbErr)
			}
			if persistErr != nil {
				e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", persistErr.Error(), &executionMS)
				return false, "", e.failApprovalGateBeforeExit(run.ID, sr.ID, parkedMS, persistErr)
			}
		}
		exitStatus, exitReason := approvalExitState(response, err)
		if reconciled {
			exitStatus, exitReason = types.StepStatusCompleted, nil
		}
		exitGate := e.db.ExitApprovalGate
		if reconciled {
			exitGate = e.db.ExitReconciledApprovalGate
		}
		if dbErr := exitGate(context.Background(), run.ID, sr.ID, exitStatus, parkedMS, exitReason); dbErr != nil {
			return false, "", e.recoverApprovalGateExit(run.ID, sr.ID, parkedMS, fmt.Errorf("exit approval gate for step %s: %w", stepName, dbErr))
		}
		if err != nil {
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", err.Error(), &executionMS)
			return false, "", fmt.Errorf("step %s: waiting for approval: %w", stepName, err)
		}
		if reconciled {
			phaseStart = time.Now()
			goto done
		}

		approvalFields := telemetry.Fields{
			"step":       string(stepName),
			"action":     string(response.action),
			"fix_review": sctx.Fixing,
		}
		if agentName := e.telemetryAgentName(); agentName != "" {
			approvalFields["agent"] = agentName
		}
		if selectedCount := selectedFindingCount(outcome.Findings, response.findingIDs); selectedCount > 0 {
			approvalFields["selected_findings_count"] = selectedCount
		}
		if response.fixOverrideReason != "" {
			approvalFields["fix_override"] = true
		}
		telemetry.Track("approval", approvalFields)

		switch response.action {
		case types.ActionApprove:
			// Approved - execution already frozen in executionMS, reset phaseStart
			// so the done label computes no additional elapsed.
			e.recordDeclinedRound(currentRoundID, outcome.Findings, stepName, roundNum)
			phaseStart = time.Now()
			goto done

		case types.ActionSkip:
			// Skip - mark step skipped and return (not an error)
			e.recordDeclinedRound(currentRoundID, outcome.Findings, stepName, roundNum)
			if err := e.db.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, finalExitCode, executionMS, logPath); err != nil {
				return false, "", fmt.Errorf("complete step %s (skip): %w", stepName, err)
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusSkipped), "", "", &executionMS)
			return false, "", nil

		case types.ActionAbort:
			e.recordDeclinedRound(currentRoundID, outcome.Findings, stepName, roundNum)
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFailed), "", "aborted by user", &executionMS)
			return false, "", fmt.Errorf("step %s: aborted by user", stepName)

		case types.ActionFix:
			telemetry.Track("fix", e.fixTelemetryFields("user", stepName, selectedFindingCount(outcome.Findings, response.findingIDs), 0))
			// Fix - mark step as fixing, resume execution timer, re-execute.
			phaseStart = time.Now()
			selectedCount := selectedFindingCount(outcome.Findings, response.findingIDs)
			writeLog(fmt.Sprintf("user-fix round starting after round %d (%d %s selected)", roundNum, selectedCount, pluralize(selectedCount, "finding", "findings")))
			sctx.Fixing = true
			selectedFindings := filterFindingsJSON(outcome.Findings, response.findingIDs)
			mergedFindings := mergeUserOverridesJSON(selectedFindings, response.instructions, response.addedFindings)
			sctx.PreviousFindings = mergedFindings
			nextTrigger = "auto_fix"
			if currentRoundID != "" {
				allSelectedIDs := combineSelectedFindingIDs(response.findingIDs, mergedFindings)
				if idsJSON := marshalFindingIDs(allSelectedIDs); idsJSON != "" {
					var userFindingsJSON *string
					if mergedFindings != "" && mergedFindings != selectedFindings {
						userFindingsJSON = &mergedFindings
					}
					selectionSource := db.RoundSelectionSourceUser
					if response.fixOverrideReason != "" {
						selectionSource = db.RoundSelectionSourceUserOverride
					}
					if dbErr := e.db.SetStepRoundUserDecision(currentRoundID, &idsJSON, selectionSource, userFindingsJSON); dbErr != nil {
						slog.Warn("failed to record user decision", "step", stepName, "round", roundNum, "error", dbErr)
					}
				}
			}
			e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(types.StepStatusFixing), "", "", nil)
			slog.Info("step fix requested, re-executing", "step", stepName)
			continue // loop back to step.Execute
		}
	}

done:
	// Mark step completed with execution-only timing.
	durationMS := executionMS + time.Since(phaseStart).Milliseconds()
	if durationOverrideMS > 0 {
		durationMS = durationOverrideMS
	}
	status := types.StepStatusCompleted
	if stepSkipped {
		status = types.StepStatusSkipped
	}
	// A review round's captured head becomes authority only when the review
	// actually completes. Parked outcomes stay in the loop above, failures
	// return earlier, and skipped reviews deliberately leave the binding empty.
	// Completion and authority replacement are one DB transaction.
	if stepName == types.StepReview && status == types.StepStatusCompleted && reviewApprovedHeadSHA != "" {
		if err := e.db.CompleteReviewStep(sr.ID, run.ID, reviewApprovedHeadSHA, finalExitCode, durationMS, logPath); err != nil {
			return false, "", fmt.Errorf("complete step %s: %w", stepName, err)
		}
		reviewedHead := reviewApprovedHeadSHA
		run.ReviewApprovedHeadSHA = &reviewedHead
		ClearUncertifiedPipelineRangeIfCertified(ctx, e.db, repo.ID, run.Branch, reviewedHead, workDir)
	} else if err := e.db.CompleteStepWithStatus(sr.ID, status, finalExitCode, durationMS, logPath); err != nil {
		return false, "", fmt.Errorf("complete step %s: %w", stepName, err)
	}
	e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, stepName, string(status), "", "", &durationMS)
	return skipRemaining, restartFrom, nil
}

// recordDeclinedRound persists an approve, skip, or abort resolution as a real
// decision instead of leaving no trace.
//
// Before this existed, those three resolutions wrote no finding-level state at
// all, so a round where the human read a blocking finding and said "ship it as
// is" was byte-identical to a round with no findings. Nothing downstream could
// tell the two apart, and the only durable statement of what the change must do
// stayed the user-intent prose - which is how a later step could re-derive and
// re-apply the very change the human had just declined.
//
// The decline is stored the way a partial selection already stores one: as the
// complement of selected_finding_ids. Writing an explicit empty array with the
// user_declined source is what makes "selected nothing" representable, since a
// NULL column means "no decision was recorded".
//
// Best effort by design. This is advisory prompt context for later steps, so a
// failed write degrades to today's behavior and must never fail the run.
func (e *Executor) recordDeclinedRound(roundID, findingsJSON string, stepName types.StepName, roundNum int) {
	if e == nil || e.db == nil || roundID == "" {
		return
	}
	if findingsCount(findingsJSON) == 0 {
		// Nothing was declined, so there is no decision to record.
		return
	}
	if err := e.db.SetStepRoundDeclined(roundID); err != nil {
		slog.Warn("failed to record declined findings", "step", stepName, "round", roundNum, "error", err)
	}
}

func roundInsertID(_ string, inserted *db.StepRound, err error) string {
	if err != nil || inserted == nil {
		return ""
	}
	return inserted.ID
}

type gateStepBoundaryAgent struct {
	inner agent.Agent
	phase types.StepName
}

func (a *gateStepBoundaryAgent) Name() string { return a.inner.Name() }

func (a *gateStepBoundaryAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	opts.Prompt = gateguidance.PromptBoundary(string(a.phase)) + opts.Prompt
	return a.inner.Run(ctx, opts)
}

func (a *gateStepBoundaryAgent) Close() error { return a.inner.Close() }

func (a *gateStepBoundaryAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *gateStepBoundaryAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *gateStepBoundaryAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *gateStepBoundaryAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

type lifecycleAgent struct {
	inner       agent.Agent
	onLifecycle func(agent.LifecycleEvent)
}

func (a *lifecycleAgent) Name() string {
	return a.inner.Name()
}

func (a *lifecycleAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	previous := opts.OnLifecycle
	opts.OnLifecycle = func(event agent.LifecycleEvent) {
		if previous != nil {
			previous(event)
		}
		if a.onLifecycle != nil {
			a.onLifecycle(event)
		}
	}
	return a.inner.Run(ctx, opts)
}

func (a *lifecycleAgent) Close() error {
	return a.inner.Close()
}

// SupportsSessionResume forwards the wrapped adapter's session capability so
// wrapping never hides it from the review loop's session manager.
func (a *lifecycleAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *lifecycleAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *lifecycleAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

const (
	maxStepActivityText          = 240
	stepActivityThrottleInterval = time.Second
)

func stepActivityFromLog(text string) string {
	end := len(text)
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:end])
		if !unicode.IsSpace(r) {
			break
		}
		end -= size
	}
	if end == 0 {
		return ""
	}
	start := strings.LastIndexByte(text[:end], '\n') + 1
	line := strings.TrimSpace(text[start:end])
	return "log: " + truncateActivity(line)
}

func truncateActivity(text string) string {
	if len(text) <= maxStepActivityText {
		return text
	}
	runeCount := 0
	for i := range text {
		if runeCount == maxStepActivityText {
			return text[:i] + "..."
		}
		runeCount++
	}
	return text
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func agentTransientParkReason(err *agent.TransientError) string {
	agentName := "agent"
	label := "transient provider failure"
	if err != nil {
		if strings.TrimSpace(err.Agent) != "" {
			agentName = strings.TrimSpace(err.Agent)
		}
		if strings.TrimSpace(err.Label) != "" {
			label = strings.TrimSpace(err.Label)
		}
	}
	reason := fmt.Sprintf("agent provider/transient failure: %s %s after retries; resume with `no-mistakes axi respond --action retry` to retry this step", agentName, label)
	if err != nil && err.Err != nil {
		reason += fmt.Sprintf(" (last error: %v)", err.Err)
	}
	return reason
}

// approvalOnlyStep prevents an unrelated external reconciliation hook from
// clearing an operator-owned transient retry gate.
type approvalOnlyStep struct{ Step }

func (e *Executor) reviewFixRoundCapReached(stepName types.StepName, stepResultID string) (bool, int, int, error) {
	if stepName != types.StepReview || e.config == nil {
		return false, 0, 0, nil
	}
	max := e.config.Review.MaxFixRounds
	if max < 0 {
		return false, 0, max, fmt.Errorf("invalid review.max_fix_rounds: %d (must be >= 0)", max)
	}
	if max == 0 {
		return false, 0, max, nil
	}
	count, err := e.db.CountStepFixRounds(stepResultID)
	if err != nil {
		return false, 0, max, err
	}
	return count >= max, count, max, nil
}

// waitForApprovalOrReconcile blocks until a user action arrives, the parked
// gate's external source of truth makes it obsolete, or the context is
// cancelled. Reconciliation runs synchronously under a bounded child context,
// so no watcher goroutine can outlive approval, cancellation, or shutdown.
// The caller must set e.waiting and e.waitingStep before calling this method.
func (e *Executor) waitForApprovalOrReconcile(ctx context.Context, step Step, sctx *StepContext, immediate bool) (approvalResponse, bool, error) {
	defer e.clearApprovalWaitState()

	if _, ok := step.(ApprovalGateReconciler); !ok {
		select {
		case response := <-e.approvalCh:
			return response, false, nil
		case <-ctx.Done():
			return approvalResponse{}, false, context.Cause(ctx)
		}
	}

	delay := e.gateReconcileInterval
	if immediate {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case response := <-e.approvalCh:
			return response, false, nil
		case <-ctx.Done():
			return approvalResponse{}, false, context.Cause(ctx)
		case <-timer.C:
			resolved, err := e.reconcileApprovalGate(ctx, step, sctx)
			if resolved {
				if e.claimGateReconciliation() {
					return approvalResponse{}, true, nil
				}
				return <-e.approvalCh, false, nil
			}
			if errors.Is(err, ErrFatalGateReconciliation) {
				return approvalResponse{}, false, err
			}
			if err != nil && ctx.Err() == nil {
				if sctx != nil && sctx.Log != nil {
					sctx.Log(fmt.Sprintf("warning: could not reconcile parked %s gate; preserving it: %v", step.Name(), err))
				} else {
					slog.Warn("could not reconcile parked approval gate; preserving it", "step", step.Name(), "error", err)
				}
			}
			timer.Reset(e.gateReconcileInterval)
		}
	}
}

func (e *Executor) claimGateReconciliation() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.waiting {
		return false
	}
	e.waiting = false
	e.waitingStep = ""
	return true
}

func (e *Executor) reconcileApprovalGate(ctx context.Context, step Step, sctx *StepContext) (bool, error) {
	reconciler, ok := step.(ApprovalGateReconciler)
	if !ok {
		return false, nil
	}
	timeout := e.gateReconcileTimeout
	if timeout <= 0 {
		timeout = defaultGateReconcileTimeout
	}
	reconcileCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	copyCtx := *sctx
	copyCtx.Ctx = reconcileCtx
	return reconciler.ReconcileApprovalGate(&copyCtx)
}

func (e *Executor) clearApprovalWaitState() {
	e.mu.Lock()
	e.waiting = false
	e.waitingStep = ""
	e.waitingStepResultID = ""
	e.waitingAgentRetry = false
	e.waitingAtFixRoundCap = false
	e.waitingFixRoundCount = 0
	e.waitingMaxFixRounds = 0
	// Respond queues while holding the same mutex, so draining here cannot
	// race with a late response being enqueued after publication failure or
	// context cancellation.
	select {
	case <-e.approvalCh:
	default:
	}
	e.mu.Unlock()
}

func approvalExitState(response approvalResponse, waitErr error) (types.StepStatus, *string) {
	if waitErr != nil {
		reason := waitErr.Error()
		return types.StepStatusFailed, &reason
	}
	switch response.action {
	case types.ActionApprove:
		return types.StepStatusCompleted, nil
	case types.ActionSkip:
		return types.StepStatusSkipped, nil
	case types.ActionAbort:
		reason := "aborted by user"
		return types.StepStatusFailed, &reason
	case types.ActionFix:
		return types.StepStatusFixing, nil
	case types.ActionRetry:
		return types.StepStatusRunning, nil
	default:
		reason := fmt.Sprintf("unsupported approval action %q", response.action)
		return types.StepStatusFailed, &reason
	}
}

func (e *Executor) failGatePublication(stepName types.StepName, stepResultID string, durationMS int64, publishErr error) error {
	if failErr := e.db.FailStep(stepResultID, publishErr.Error(), durationMS); failErr != nil {
		slog.Warn("failed to mark step as failed after gate publication error", "step", stepName, "error", failErr)
	}
	return publishErr
}

func (e *Executor) recoverApprovalGateExit(runID, stepResultID string, parkedMS int64, exitErr error) error {
	if cleanupErr := e.db.FailApprovalGate(context.Background(), runID, stepResultID, parkedMS, exitErr.Error()); cleanupErr != nil {
		return fmt.Errorf("%w; terminal gate cleanup failed: %v", exitErr, cleanupErr)
	}
	return exitErr
}

func (e *Executor) failApprovalGateBeforeExit(runID, stepResultID string, parkedMS int64, primaryErr error) error {
	reason := primaryErr.Error()
	if exitErr := e.db.ExitApprovalGate(context.Background(), runID, stepResultID, types.StepStatusFailed, parkedMS, &reason); exitErr != nil {
		cleanupErr := e.recoverApprovalGateExit(runID, stepResultID, parkedMS, fmt.Errorf("exit approval gate after %v: %w", primaryErr, exitErr))
		return fmt.Errorf("%w; approval gate cleanup failed: %v", primaryErr, cleanupErr)
	}
	return primaryErr
}

// failRun marks a run as failed and returns the error.
// It accepts an optional context; if the context was cancelled with a cause,
// the cause message is used as the run's error (more informative than "context canceled").
func (e *Executor) failRun(run *db.Run, repo *db.Repo, err error, ctxs ...context.Context) error {
	errMsg := err.Error()
	for _, ctx := range ctxs {
		if cause := context.Cause(ctx); cause != nil && cause != context.Canceled {
			errMsg = cause.Error()
			break
		}
	}
	runStatus := types.RunFailed
	if errMsg == types.RunCancelReasonAbortedByUser || errMsg == types.RunCancelReasonSuperseded {
		runStatus = types.RunCancelled
	}
	verifiedHead, verified := e.reconcileTerminalRunHead(run)
	var dbErr error
	if verified {
		dbErr = e.db.UpdateRunErrorStatusWithVerifiedHead(run.ID, errMsg, runStatus, verifiedHead)
	} else {
		dbErr = e.db.UpdateRunErrorStatus(run.ID, errMsg, runStatus)
	}
	if dbErr != nil {
		slog.Error("failed to update run error status", "run", run.ID, "error", dbErr)
	} else if verified {
		run.HeadSHA = verifiedHead
	}
	run.Status = runStatus
	run.Error = &errMsg
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	return err
}

func (e *Executor) completeRun(run *db.Run, repo *db.Repo) error {
	verifiedHead, verified := e.reconcileTerminalRunHead(run)
	var err error
	if verified {
		err = e.db.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCompleted, verifiedHead)
	} else {
		err = e.db.UpdateRunStatus(run.ID, types.RunCompleted)
	}
	if err != nil {
		return err
	}
	if verified {
		run.HeadSHA = verifiedHead
	}
	run.Status = types.RunCompleted
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	return nil
}

func (e *Executor) reconcileTerminalRunHead(run *db.Run) (string, bool) {
	if run == nil || strings.TrimSpace(e.workDir) == "" {
		return "", false
	}
	recordedRun, err := e.db.GetRun(run.ID)
	if err != nil || recordedRun == nil {
		slog.Warn("failed to load run head before terminalization", "run", run.ID, "error", err)
		return "", false
	}
	recorded := strings.TrimSpace(recordedRun.HeadSHA)
	if recorded == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observed, err := git.HeadSHA(ctx, e.workDir)
	if err != nil {
		slog.Warn("failed to resolve worktree head before terminalization", "run", run.ID, "error", err)
		return "", false
	}
	observed = strings.TrimSpace(observed)
	if observed == "" {
		return "", false
	}
	if observed == recorded {
		if !e.preserveUnpublishedTerminalHead(ctx, recordedRun, observed) {
			return "", false
		}
		return recorded, true
	}
	if _, err := git.Run(ctx, e.workDir, "merge-base", "--is-ancestor", recorded, observed); err != nil {
		slog.Warn("worktree head is not a verified descendant before terminalization", "run", run.ID, "error", err)
		return "", false
	}
	if !e.preserveUnpublishedTerminalHead(ctx, recordedRun, observed) {
		return "", false
	}
	return observed, true
}

func (e *Executor) preserveUnpublishedTerminalHead(ctx context.Context, run *db.Run, head string) bool {
	if run == nil || head == "" {
		return false
	}
	published := ""
	if run.LastPushedSHA != nil {
		published = *run.LastPushedSHA
	}
	if published == "" {
		if run.SubmittedHeadSHA != nil {
			published = *run.SubmittedHeadSHA
		}
	}
	if head == published {
		return true
	}
	if err := custody.PreserveRecoveryHead(ctx, e.workDir, run.ID, head); err != nil {
		slog.Warn("failed to anchor unpublished terminal head", "run", run.ID, "head", head, "error", err)
		return false
	}
	return true
}

// --- event helpers ---

func (e *Executor) emitRunEvent(eventType ipc.EventType, run *db.Run, repo *db.Repo) {
	status := string(run.Status)
	event := ipc.Event{
		Type:   eventType,
		RunID:  run.ID,
		RepoID: repo.ID,
		Status: &status,
		Branch: &run.Branch,
		Error:  run.Error,
		PRURL:  run.PRURL,
	}
	e.onEvent(event)
}

func (e *Executor) emitCIReadinessEvent(run *db.Run, repo *db.Repo, ready, declaredNoCI bool) {
	declaredNoCI = ready && declaredNoCI
	e.onEvent(ipc.Event{
		Type:        ipc.EventCIReadinessChanged,
		RunID:       run.ID,
		RepoID:      repo.ID,
		CIReady:     &ready,
		CIReadyNoCI: &declaredNoCI,
	})
}

func (e *Executor) emitStepEvent(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string) {
	e.emitStepEventWithFindings(eventType, run, repo, stepName, status, "")
}

func (e *Executor) emitStepEventWithFindings(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string) {
	e.emitStepEventWithFindingsAndError(eventType, run, repo, stepName, status, findings, "", nil)
}

func (e *Executor) emitStepEventWithFindingsAndError(eventType ipc.EventType, run *db.Run, repo *db.Repo, stepName types.StepName, status string, findings string, errMsg string, durationMS *int64) {
	event := ipc.Event{
		Type:       eventType,
		RunID:      run.ID,
		RepoID:     repo.ID,
		StepName:   &stepName,
		Status:     &status,
		DurationMS: durationMS,
	}
	stats := e.findingStatsForStep(run.ID, stepName)
	if stats.ReportedFindings > 0 || stats.FixedFindings > 0 {
		reported := stats.ReportedFindings
		fixed := stats.FixedFindings
		event.ReportedFindings = &reported
		event.FixedFindings = &fixed
	}
	if errMsg != "" {
		event.Error = &errMsg
	}
	if findings != "" {
		event.Findings = &findings
	}
	e.onEvent(event)
	if !shouldTrackStepTelemetry(eventType, status) {
		return
	}

	fields := telemetry.Fields{
		"event":  string(eventType),
		"step":   string(stepName),
		"status": status,
	}
	if agentName := e.telemetryAgentName(); agentName != "" {
		fields["agent"] = agentName
	}
	if durationMS != nil {
		fields["duration_ms"] = *durationMS
	}
	if findings != "" {
		fields["findings_count"] = findingsCount(findings)
	}
	telemetry.Track("step", fields)
}

func (e *Executor) findingStatsForStep(runID string, stepName types.StepName) db.StepStats {
	steps, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return db.StepStats{StepName: stepName}
	}
	for _, step := range steps {
		if step.StepName != stepName {
			continue
		}
		stats, err := e.db.StepFindingStats(step)
		if err != nil {
			return db.StepStats{StepName: stepName}
		}
		return stats
	}
	return db.StepStats{StepName: stepName}
}

func shouldTrackStepTelemetry(eventType ipc.EventType, status string) bool {
	if eventType != ipc.EventStepCompleted {
		return false
	}
	switch types.StepStatus(status) {
	case types.StepStatusAwaitingApproval, types.StepStatusAwaitingRetry, types.StepStatusFixReview, types.StepStatusAwaitingTriage, types.StepStatusFailed:
		return true
	default:
		return false
	}
}

func (e *Executor) emitLogChunk(run *db.Run, repo *db.Repo, stepName types.StepName, content string) {
	e.onEvent(ipc.Event{
		Type:     ipc.EventLogChunk,
		RunID:    run.ID,
		RepoID:   repo.ID,
		StepName: &stepName,
		Content:  &content,
	})
}

func (e *Executor) telemetryAgentName() string {
	if e.config == nil || e.config.Agent == "" {
		return ""
	}
	return string(e.config.Agent)
}

func (e *Executor) fixTelemetryFields(source string, stepName types.StepName, selectedCount int, attempt int) telemetry.Fields {
	fields := telemetry.Fields{
		"source":                  source,
		"step":                    string(stepName),
		"selected_findings_count": selectedCount,
	}
	if agentName := e.telemetryAgentName(); agentName != "" {
		fields["agent"] = agentName
	}
	if attempt > 0 {
		fields["attempt"] = attempt
	}
	return fields
}

func findingsCount(raw string) int {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return 0
	}
	return len(findings.Items)
}

func selectedFindingCount(raw string, ids []string) int {
	if len(ids) > 0 {
		return len(ids)
	}
	return findingsCount(raw)
}
