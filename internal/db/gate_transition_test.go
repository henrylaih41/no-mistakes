package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func openGateTransitionTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, path
}

func gateTransitionFixture(t *testing.T, database *DB) (*Run, *StepResult) {
	t.Helper()
	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "main", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(step.ID, types.StepStatusFixing); err != nil {
		t.Fatal(err)
	}
	return run, step
}

func assertGatePair(t *testing.T, run *Run, step *StepResult, wantGate bool) {
	t.Helper()
	markerPublished := run.AwaitingAgentSince != nil
	statusPublished := step.Status == types.StepStatusFixReview
	if markerPublished != statusPublished {
		t.Fatalf("split gate publication: awaiting_agent_since=%v step_status=%s", run.AwaitingAgentSince, step.Status)
	}
	if markerPublished != wantGate {
		t.Fatalf("gate published=%v, want %v (status=%s)", markerPublished, wantGate, step.Status)
	}
}

func TestEnterApprovalGate_AtomicallyPublishesMarkerAndStepStatus(t *testing.T) {
	database, path := openGateTransitionTestDB(t)
	run, step := gateTransitionFixture(t, database)
	observer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	betweenUpdates := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTransition := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseTransition)
	enterApprovalGateBetweenUpdatesHook = func() {
		close(betweenUpdates)
		<-release
	}
	t.Cleanup(func() { enterApprovalGateBetweenUpdatesHook = nil })

	type result struct {
		since int64
		err   error
	}
	done := make(chan result, 1)
	go func() {
		since, err := database.EnterApprovalGate(context.Background(), run.ID, step.ID, types.StepStatusFixReview, 137, nil)
		done <- result{since: since, err: err}
	}()

	select {
	case <-betweenUpdates:
	case <-time.After(2 * time.Second):
		t.Fatal("approval gate transition did not reach the between-updates hook")
	}

	observedRun, err := observer.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observedStep, err := observer.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertGatePair(t, observedRun, observedStep, false)

	releaseTransition()
	transition := <-done
	if transition.err != nil {
		t.Fatal(transition.err)
	}
	if transition.since == 0 {
		t.Fatal("EnterApprovalGate returned a zero marker timestamp")
	}

	observedRun, err = observer.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observedStep, err = observer.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertGatePair(t, observedRun, observedStep, true)
	if observedStep.DurationMS == nil || *observedStep.DurationMS != 137 {
		t.Fatalf("duration_ms = %v, want 137", observedStep.DurationMS)
	}
}

func TestEnterApprovalGate_RollsBackMarkerWhenStepUpdateFails(t *testing.T) {
	database, _ := openGateTransitionTestDB(t)
	run, step := gateTransitionFixture(t, database)

	trigger := fmt.Sprintf(`
		CREATE TEMP TRIGGER abort_gate_status
		BEFORE UPDATE OF status ON step_results
		WHEN NEW.id = '%s' AND NEW.status = '%s'
		BEGIN
			SELECT RAISE(ABORT, 'forced gate status failure');
		END`, step.ID, types.StepStatusFixReview)
	if _, err := database.sql.Exec(trigger); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := database.EnterApprovalGate(ctx, run.ID, step.ID, types.StepStatusFixReview, 211, nil)
	if err == nil || !strings.Contains(err.Error(), "forced gate status failure") {
		t.Fatalf("EnterApprovalGate error = %v, want forced gate status failure", err)
	}

	observedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observedStep, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observedRun.AwaitingAgentSince != nil {
		t.Fatalf("awaiting_agent_since = %d after rollback, want nil", *observedRun.AwaitingAgentSince)
	}
	if observedStep.Status != types.StepStatusFixing {
		t.Fatalf("step status = %s after rollback, want fixing", observedStep.Status)
	}
	if observedStep.DurationMS != nil {
		t.Fatalf("duration_ms = %d after rollback, want nil", *observedStep.DurationMS)
	}
}

func TestGateLogFieldsUsesDeviceAndInodeIdentity(t *testing.T) {
	database, path := openGateTransitionTestDB(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fields := database.gateLogFields("transition", "enter", "run", "step", types.StepStatusFixReview)
	var identity string
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "db_identity" {
			identity, _ = fields[i+1].(string)
			break
		}
	}
	if identity == "" {
		t.Fatal("db_identity field is empty")
	}

	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer {
		stat = stat.Elem()
	}
	if stat.IsValid() && stat.Kind() == reflect.Struct {
		dev := stat.FieldByName("Dev")
		ino := stat.FieldByName("Ino")
		if dev.IsValid() && ino.IsValid() && dev.CanInterface() && ino.CanInterface() {
			want := fmt.Sprintf("%s|dev=%v|ino=%v", database.path, dev.Interface(), ino.Interface())
			if identity != want {
				t.Fatalf("db_identity = %q, want %q", identity, want)
			}
			return
		}
	}
	if identity != database.path {
		t.Fatalf("db_identity = %q, want path fallback %q", identity, database.path)
	}
}

func TestExitApprovalGate_AtomicallyClearsMarkerAndStepStatus(t *testing.T) {
	database, path := openGateTransitionTestDB(t)
	run, step := gateTransitionFixture(t, database)
	if _, err := database.EnterApprovalGate(context.Background(), run.ID, step.ID, types.StepStatusFixReview, 144, nil); err != nil {
		t.Fatal(err)
	}
	observer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	betweenUpdates := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTransition := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseTransition)
	exitApprovalGateBetweenUpdatesHook = func() {
		close(betweenUpdates)
		<-release
	}
	t.Cleanup(func() { exitApprovalGateBetweenUpdatesHook = nil })

	done := make(chan error, 1)
	go func() {
		done <- database.ExitApprovalGate(context.Background(), run.ID, step.ID, types.StepStatusFixing, 250, nil)
	}()

	select {
	case <-betweenUpdates:
	case <-time.After(2 * time.Second):
		t.Fatal("approval gate exit did not reach the between-updates hook")
	}
	observedRun, err := observer.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observedStep, err := observer.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertGatePair(t, observedRun, observedStep, true)

	releaseTransition()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	observedRun, err = observer.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observedStep, err = observer.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertGatePair(t, observedRun, observedStep, false)
	if observedRun.ParkedMS != 250 {
		t.Fatalf("parked_ms = %d, want 250", observedRun.ParkedMS)
	}
}

func TestGetRunSnapshot_IsGateConsistentDuringConcurrentTransition(t *testing.T) {
	database, path := openGateTransitionTestDB(t)
	run, step := gateTransitionFixture(t, database)
	observer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	runRead := make(chan struct{})
	release := make(chan struct{})
	var hookOnce sync.Once
	getRunSnapshotAfterRunReadHook = func() {
		hookOnce.Do(func() {
			close(runRead)
			<-release
		})
	}
	t.Cleanup(func() { getRunSnapshotAfterRunReadHook = nil })

	type snapshotResult struct {
		snapshot *RunSnapshot
		err      error
	}
	done := make(chan snapshotResult, 1)
	go func() {
		snapshot, err := observer.GetRunSnapshot(context.Background(), run.ID)
		done <- snapshotResult{snapshot: snapshot, err: err}
	}()

	select {
	case <-runRead:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot did not reach the after-run-read hook")
	}

	if _, err := database.EnterApprovalGate(context.Background(), run.ID, step.ID, types.StepStatusFixReview, 89, nil); err != nil {
		t.Fatal(err)
	}
	close(release)

	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.snapshot.Steps) != 1 {
		t.Fatalf("snapshot steps = %d, want 1", len(result.snapshot.Steps))
	}
	assertGatePair(t, result.snapshot.Run, result.snapshot.Steps[0], false)

	committed, err := observer.GetRunSnapshot(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.Steps) != 1 {
		t.Fatalf("committed snapshot steps = %d, want 1", len(committed.Steps))
	}
	assertGatePair(t, committed.Run, committed.Steps[0], true)
}
