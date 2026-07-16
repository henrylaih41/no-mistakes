package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const gateDBTimeout = 10 * time.Second

// Test hooks make transaction boundaries deterministic in concurrency tests.
// Production leaves them nil.
var (
	enterApprovalGateBetweenUpdatesHook func()
	exitApprovalGateBetweenUpdatesHook  func()
	getRunSnapshotAfterRunReadHook      func()
)

// RunSnapshot is a run row and its step rows from one SQLite read snapshot.
type RunSnapshot struct {
	Run   *Run
	Steps []*StepResult
}

// EnterApprovalGate atomically publishes the run marker and durable step gate.
// A failure rolls back both writes so the executor can fail instead of waiting
// at a gate that status readers cannot see.
func (d *DB) EnterApprovalGate(parent context.Context, runID, stepResultID string, status types.StepStatus, durationMS int64, reason *string) (since int64, err error) {
	if !isApprovalGateStatus(status) {
		return 0, fmt.Errorf("enter approval gate with non-gate status %q", status)
	}
	ctx, cancel := boundedGateContext(parent)
	defer cancel()

	transitionID := newID()
	started := time.Now()
	var markerSince int64
	fields := d.gateLogFields(transitionID, "enter", runID, stepResultID, status)
	defer func() {
		fields = append(fields,
			"awaiting_agent_since", markerSince,
			"elapsed_ms", time.Since(started).Milliseconds(),
		)
		if err != nil {
			slog.Error("approval gate transition failed", append(fields, "error", err)...)
		}
	}()

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin approval gate transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	markerSince = now()
	runResult, err := tx.ExecContext(ctx,
		`UPDATE runs SET awaiting_agent_since = ?, updated_at = ? WHERE id = ?`,
		markerSince, markerSince, runID,
	)
	if err != nil {
		return 0, fmt.Errorf("publish approval gate marker: %w", err)
	}
	if err := requireOneRow(runResult, "publish approval gate marker"); err != nil {
		return 0, err
	}

	if enterApprovalGateBetweenUpdatesHook != nil {
		enterApprovalGateBetweenUpdatesHook()
	}

	activityAt := now()
	activity := fmt.Sprintf("status: %s", status)
	var stepResult sql.Result
	if reason == nil {
		stepResult, err = tx.ExecContext(ctx,
			`UPDATE step_results SET status = ?, duration_ms = ?, last_activity_at = ?, last_activity = ? WHERE id = ? AND run_id = ?`,
			status, durationMS, activityAt, activity, stepResultID, runID,
		)
	} else {
		stepResult, err = tx.ExecContext(ctx,
			`UPDATE step_results SET status = ?, duration_ms = ?, error = ?, completed_at = NULL, last_activity_at = ?, last_activity = ? WHERE id = ? AND run_id = ?`,
			status, durationMS, *reason, activityAt, activity, stepResultID, runID,
		)
	}
	if err != nil {
		return 0, fmt.Errorf("publish approval gate step status: %w", err)
	}
	if err := requireOneRow(stepResult, "publish approval gate step status"); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit approval gate transaction: %w", err)
	}
	slog.Info("approval gate transition committed", append(fields,
		"awaiting_agent_since", markerSince,
		"run_rows_affected", 1,
		"step_rows_affected", 1,
		"elapsed_ms", time.Since(started).Milliseconds(),
	)...)
	return markerSince, nil
}

// ExitApprovalGate atomically clears the run marker, accumulates parked time,
// and moves the step to its next durable state.
func (d *DB) ExitApprovalGate(parent context.Context, runID, stepResultID string, status types.StepStatus, parkedMS int64, reason *string) (err error) {
	ctx, cancel := boundedGateContext(parent)
	defer cancel()
	if parkedMS < 0 {
		parkedMS = 0
	}

	transitionID := newID()
	started := time.Now()
	fields := d.gateLogFields(transitionID, "exit", runID, stepResultID, status)
	var since int64
	defer func() {
		fields = append(fields,
			"awaiting_agent_since", since,
			"elapsed_ms", time.Since(started).Milliseconds(),
		)
		if err != nil {
			slog.Error("approval gate transition failed", append(fields, "error", err)...)
		}
	}()

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin approval gate exit transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if scanErr := tx.QueryRowContext(ctx, `SELECT awaiting_agent_since FROM runs WHERE id = ?`, runID).Scan(&since); scanErr != nil {
		return fmt.Errorf("read approval gate marker: %w", scanErr)
	}

	activityAt := now()
	activity := fmt.Sprintf("status: %s", status)
	terminal := status == types.StepStatusCompleted || status == types.StepStatusSkipped || status == types.StepStatusFailed
	var completedAt any
	if terminal {
		completedAt = activityAt
	}
	var errorValue any
	if reason != nil {
		errorValue = *reason
	}
	stepResult, err := tx.ExecContext(ctx,
		`UPDATE step_results
		 SET status = ?, error = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL
		 WHERE id = ? AND run_id = ? AND status IN (?, ?, ?, ?)`,
		status, errorValue, completedAt, activityAt, activity, stepResultID, runID,
		types.StepStatusAwaitingApproval, types.StepStatusFixReview,
		types.StepStatusAwaitingRetry, types.StepStatusAwaitingTriage,
	)
	if err != nil {
		return fmt.Errorf("publish approval gate exit step status: %w", err)
	}
	if err := requireOneRow(stepResult, "publish approval gate exit step status"); err != nil {
		return err
	}
	if exitApprovalGateBetweenUpdatesHook != nil {
		exitApprovalGateBetweenUpdatesHook()
	}

	runResult, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET awaiting_agent_since = NULL, parked_ms = COALESCE(parked_ms, 0) + ?, updated_at = ?
		 WHERE id = ? AND awaiting_agent_since IS NOT NULL`,
		parkedMS, now(), runID,
	)
	if err != nil {
		return fmt.Errorf("clear approval gate marker: %w", err)
	}
	if err := requireOneRow(runResult, "clear approval gate marker"); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit approval gate exit transaction: %w", err)
	}
	slog.Info("approval gate transition committed", append(fields,
		"awaiting_agent_since", since,
		"parked_ms", parkedMS,
		"run_rows_affected", 1,
		"step_rows_affected", 1,
		"elapsed_ms", time.Since(started).Milliseconds(),
	)...)
	return nil
}

// FailApprovalGate is the terminal fallback when an atomic gate exit fails.
// It makes a second bounded transaction that fails the step and run together,
// clears the marker, and preserves parked time. This prevents a failed run from
// retaining a live-looking gate after the original exit transaction rolls back.
func (d *DB) FailApprovalGate(parent context.Context, runID, stepResultID string, parkedMS int64, reason string) (err error) {
	ctx, cancel := boundedGateContext(parent)
	defer cancel()
	if parkedMS < 0 {
		parkedMS = 0
	}

	transitionID := newID()
	started := time.Now()
	fields := d.gateLogFields(transitionID, "fail", runID, stepResultID, types.StepStatusFailed)
	defer func() {
		fields = append(fields,
			"parked_ms", parkedMS,
			"elapsed_ms", time.Since(started).Milliseconds(),
		)
		if err != nil {
			slog.Error("approval gate terminal cleanup failed", append(fields, "error", err)...)
		}
	}()

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin approval gate terminal cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ts := now()
	stepResult, err := tx.ExecContext(ctx,
		`UPDATE step_results
		 SET status = ?, error = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL
		 WHERE id = ? AND run_id = ? AND status IN (?, ?, ?, ?)`,
		types.StepStatusFailed, reason, ts, ts, "step failed: "+reason, stepResultID, runID,
		types.StepStatusAwaitingApproval, types.StepStatusFixReview,
		types.StepStatusAwaitingRetry, types.StepStatusAwaitingTriage,
	)
	if err != nil {
		return fmt.Errorf("fail approval gate step: %w", err)
	}
	if err := requireOneRow(stepResult, "fail approval gate step"); err != nil {
		return err
	}

	runResult, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET status = ?, error = ?, awaiting_agent_since = NULL,
		     parked_ms = COALESCE(parked_ms, 0) + ?, updated_at = ?
		 WHERE id = ?`,
		types.RunFailed, reason, parkedMS, ts, runID,
	)
	if err != nil {
		return fmt.Errorf("fail approval gate run: %w", err)
	}
	if err := requireOneRow(runResult, "fail approval gate run"); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit approval gate terminal cleanup: %w", err)
	}
	slog.Info("approval gate terminal cleanup committed", append(fields,
		"parked_ms", parkedMS,
		"run_rows_affected", 1,
		"step_rows_affected", 1,
		"elapsed_ms", time.Since(started).Milliseconds(),
	)...)
	return nil
}

func isApprovalGateStatus(status types.StepStatus) bool {
	switch status {
	case types.StepStatusAwaitingApproval, types.StepStatusFixReview,
		types.StepStatusAwaitingRetry, types.StepStatusAwaitingTriage:
		return true
	default:
		return false
	}
}

func boundedGateContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, gateDBTimeout)
}

func requireOneRow(result sql.Result, operation string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", operation, err)
	}
	if rows != 1 {
		return fmt.Errorf("%s affected %d rows, want 1", operation, rows)
	}
	return nil
}

func (d *DB) gateLogFields(transitionID, phase, runID, stepResultID string, status types.StepStatus) []any {
	path := d.path
	canonicalRoot := ""
	identity := path
	if path != "" {
		canonicalRoot = filepath.Dir(path)
		if info, err := os.Stat(path); err == nil {
			identity = stableFileIdentity(path, info)
		}
	}
	return []any{
		"transition_id", transitionID,
		"transition_phase", phase,
		"daemon_pid", os.Getpid(),
		"canonical_root", canonicalRoot,
		"db_path", path,
		"db_identity", identity,
		"run", runID,
		"step_result", stepResultID,
		"step_status", status,
	}
}

func stableFileIdentity(path string, info os.FileInfo) string {
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer {
		stat = stat.Elem()
	}
	if !stat.IsValid() || stat.Kind() != reflect.Struct {
		return path
	}
	dev := stat.FieldByName("Dev")
	ino := stat.FieldByName("Ino")
	if !dev.IsValid() || !ino.IsValid() || !dev.CanInterface() || !ino.CanInterface() {
		return path
	}
	return fmt.Sprintf("%s|dev=%v|ino=%v", path, dev.Interface(), ino.Interface())
}

// GetRunSnapshot returns one run and all of its steps in a single read
// transaction so a gate transition cannot be torn across the response.
func (d *DB) GetRunSnapshot(parent context.Context, runID string) (*RunSnapshot, error) {
	ctx, cancel := boundedGateContext(parent)
	defer cancel()
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin run snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	run, err := getRunWithQuery(ctx, tx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty run snapshot: %w", err)
		}
		return nil, nil
	}
	if getRunSnapshotAfterRunReadHook != nil {
		getRunSnapshotAfterRunReadHook()
	}
	steps, err := getStepsByRunWithQuery(ctx, tx, run.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit run snapshot: %w", err)
	}
	return &RunSnapshot{Run: run, Steps: steps}, nil
}

// GetRunsByRepoSnapshots returns repo runs and their steps from one read
// transaction.
func (d *DB) GetRunsByRepoSnapshots(parent context.Context, repoID string) ([]*RunSnapshot, error) {
	return d.getRunSnapshots(parent,
		`SELECT `+runColumns+` FROM runs WHERE repo_id = ? ORDER BY created_at DESC, id DESC`,
		repoID,
	)
}

// GetRunsByRepoHeadSnapshots returns exact-head runs and their steps from one
// read transaction.
func (d *DB) GetRunsByRepoHeadSnapshots(parent context.Context, repoID, branch, headSHA string) ([]*RunSnapshot, error) {
	return d.getRunSnapshots(parent,
		`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? AND head_sha = ? ORDER BY created_at DESC, id DESC`,
		repoID, branch, headSHA,
	)
}

// GetActiveRunSnapshot returns the newest active run and its steps from one
// read transaction.
func (d *DB) GetActiveRunSnapshot(parent context.Context, repoID, branch string) (*RunSnapshot, error) {
	ctx, cancel := boundedGateContext(parent)
	defer cancel()
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin active run snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `SELECT ` + runColumns + ` FROM runs WHERE repo_id = ? AND status IN ('pending', 'running') ORDER BY created_at DESC, id DESC LIMIT 1`
	args := []any{repoID}
	if branch != "" {
		query = `SELECT ` + runColumns + ` FROM runs WHERE repo_id = ? AND branch = ? AND status IN ('pending', 'running') ORDER BY created_at DESC, id DESC LIMIT 1`
		args = append(args, branch)
	}
	run, err := getRunWithQuery(ctx, tx, query, args...)
	if err != nil {
		return nil, err
	}
	if run == nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty active run snapshot: %w", err)
		}
		return nil, nil
	}
	steps, err := getStepsByRunWithQuery(ctx, tx, run.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit active run snapshot: %w", err)
	}
	return &RunSnapshot{Run: run, Steps: steps}, nil
}

type contextQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getRunWithQuery(ctx context.Context, queryer contextQuerier, query string, args ...any) (*Run, error) {
	run := &Run{}
	err := scanRun(queryer.QueryRowContext(ctx, query, args...), run)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run snapshot row: %w", err)
	}
	return run, nil
}

func getStepsByRunWithQuery(ctx context.Context, queryer contextQuerier, runID string) ([]*StepResult, error) {
	rows, err := queryer.QueryContext(ctx,
		`SELECT `+stepResultColumns+` FROM step_results WHERE run_id = ? ORDER BY step_order`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get run snapshot steps: %w", err)
	}
	defer rows.Close()
	var steps []*StepResult
	for rows.Next() {
		step := &StepResult{}
		if err := rows.Scan(&step.ID, &step.RunID, &step.StepName, &step.StepOrder, &step.Status, &step.ExitCode, &step.DurationMS, &step.LogPath, &step.FindingsJSON, &step.Error, &step.StartedAt, &step.CompletedAt, &step.LastActivityAt, &step.LastActivity, &step.AgentPID, &step.AutoFixLimit); err != nil {
			return nil, fmt.Errorf("scan run snapshot step: %w", err)
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run snapshot steps: %w", err)
	}
	return steps, nil
}

func (d *DB) getRunSnapshots(parent context.Context, query string, args ...any) ([]*RunSnapshot, error) {
	ctx, cancel := boundedGateContext(parent)
	defer cancel()
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin runs snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get run snapshot rows: %w", err)
	}
	var runs []*Run
	for rows.Next() {
		run := &Run{}
		if err := scanRun(rows, run); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan run snapshot: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close run snapshot rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run snapshot rows: %w", err)
	}

	snapshots := make([]*RunSnapshot, 0, len(runs))
	for _, run := range runs {
		steps, err := getStepsByRunWithQuery(ctx, tx, run.ID)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, &RunSnapshot{Run: run, Steps: steps})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit runs snapshot: %w", err)
	}
	return snapshots, nil
}
