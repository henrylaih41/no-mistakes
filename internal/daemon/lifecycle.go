package daemon

import (
	"context"
	"log/slog"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

const (
	worktreeCleanupActorRunManager    = "run_manager"
	worktreeCleanupActorRecovery      = "daemon_recovery"
	worktreeCleanupReasonSetupFailed  = "setup_failed"
	worktreeCleanupReasonRunFinished  = "run_finished"
	worktreeCleanupReasonRunCancelled = "run_cancelled"
	worktreeCleanupReasonStartupStale = "startup_orphan"
)

var removeGitWorktree = git.WorktreeRemove

// classifyRunCleanup maps a run context's cancellation cause to the worktree
// cleanup reason and a human-readable cause string. The cause MUST be read
// before the run finalizer cancels its own context with cancel(nil): after
// cancel(nil), context.Cause reports context.Canceled and a normally-finished
// run would be misattributed as cancelled.
func classifyRunCleanup(cause error) (reason, causeStr string) {
	if cause != nil {
		return worktreeCleanupReasonRunCancelled, cause.Error()
	}
	return worktreeCleanupReasonRunFinished, ""
}

type worktreeCleanupLog struct {
	Actor     string
	Reason    string
	RepoID    string
	RunID     string
	Path      string
	RunStatus string
	Cause     string
}

func cleanupRunWorktree(ctx context.Context, gateDir, wtDir string, meta worktreeCleanupLog) error {
	err := removeGitWorktree(ctx, gateDir, wtDir)
	attrs := meta.attrs()
	if err != nil {
		slog.Warn("worktree cleanup failed", append(attrs, "error", err)...)
		return err
	}
	slog.Info("worktree cleanup completed", attrs...)
	return nil
}

func (m worktreeCleanupLog) attrs() []any {
	attrs := []any{
		"actor", m.Actor,
		"reason", m.Reason,
		"repo_id", m.RepoID,
		"run_id", m.RunID,
		"path", m.Path,
	}
	if m.RunStatus != "" {
		attrs = append(attrs, "run_status", m.RunStatus)
	}
	if m.Cause != "" {
		attrs = append(attrs, "cause", m.Cause)
	}
	return attrs
}

type daemonExitState struct {
	mu     sync.Mutex
	reason string
}

func newDaemonExitState() *daemonExitState {
	return &daemonExitState{reason: "unknown"}
}

// set records the daemon's exit reason with set-once semantics: only the first
// recorded reason wins. The daemon's true exit cause is whatever fired first
// (a signal, an IPC stop, a fatal startup error); a later downstream cause
// triggered by that shutdown (e.g. the listener closing) must not clobber it.
func (s *daemonExitState) set(reason string) {
	if reason == "" {
		reason = "unspecified"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == "unknown" {
		s.reason = reason
	}
}

func (s *daemonExitState) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func logDaemonExit(reason string, err error, recovered any) {
	switch {
	case recovered != nil:
		slog.Error("daemon exiting", "reason", "panic", "panic", recovered)
	case err != nil:
		if reason == "unknown" {
			reason = "error"
		}
		slog.Error("daemon exiting", "reason", reason, "error", err)
	default:
		slog.Info("daemon exiting", "reason", reason)
	}
}
