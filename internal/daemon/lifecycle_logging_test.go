package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestCleanupRunWorktreeLogsActorAndReasonOnSuccess(t *testing.T) {
	var logs bytes.Buffer
	restore := captureLifecycleLogs(&logs)
	defer restore()

	oldRemove := removeGitWorktree
	removeGitWorktree = func(context.Context, string, string) error {
		return nil
	}
	defer func() { removeGitWorktree = oldRemove }()

	err := cleanupRunWorktree(context.Background(), "/gate/repo.git", "/worktrees/repo/run", worktreeCleanupLog{
		Actor:     worktreeCleanupActorRunManager,
		Reason:    worktreeCleanupReasonRunFinished,
		RepoID:    "repo",
		RunID:     "run",
		Path:      "/worktrees/repo/run",
		RunStatus: "completed",
		Cause:     "context canceled",
	})
	if err != nil {
		t.Fatalf("cleanupRunWorktree: %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		`msg="worktree cleanup completed"`,
		`actor=run_manager`,
		`reason=run_finished`,
		`repo_id=repo`,
		`run_id=run`,
		`path=/worktrees/repo/run`,
		`run_status=completed`,
		`cause="context canceled"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cleanup log missing %q:\n%s", want, got)
		}
	}
}

func TestRunWithOptionsLogsExitReasonOnStartupError(t *testing.T) {
	var logs bytes.Buffer
	restore := captureLifecycleLogs(&logs)
	defer restore()

	tmpDir := t.TempDir()
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	oldCreate := createDaemonPIDTempFile
	createDaemonPIDTempFile = func(string, string) (*os.File, error) {
		return nil, errors.New("pid temp unavailable")
	}
	defer func() { createDaemonPIDTempFile = oldCreate }()

	err = RunWithOptions(p, d, nil)
	if err == nil {
		t.Fatal("RunWithOptions succeeded, want pid-file error")
	}

	got := logs.String()
	for _, want := range []string{
		`msg="daemon exiting"`,
		`level=ERROR`,
		`reason="write pid file"`,
		`error="write pid file: create pid temp file: pid temp unavailable"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("daemon exit log missing %q:\n%s", want, got)
		}
	}
}

func TestLogDaemonExitRecordsPanicReason(t *testing.T) {
	var logs bytes.Buffer
	restore := captureLifecycleLogs(&logs)
	defer restore()

	logDaemonExit("unknown", nil, "boom")

	got := logs.String()
	for _, want := range []string{
		`msg="daemon exiting"`,
		`level=ERROR`,
		`reason=panic`,
		`panic=boom`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("panic exit log missing %q:\n%s", want, got)
		}
	}
}

func TestRecoverOnStartupLogsOrphanWorktreeActorAndReason(t *testing.T) {
	var logs bytes.Buffer
	restore := captureLifecycleLogs(&logs)
	defer restore()

	tmpDir := t.TempDir()
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	orphanDir := p.WorktreeDir("repo-1", "run-1")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "marker.txt"), []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldRemove := removeGitWorktree
	removeGitWorktree = func(context.Context, string, string) error {
		return nil
	}
	defer func() { removeGitWorktree = oldRemove }()

	recoverOnStartup(d, p)

	got := logs.String()
	for _, want := range []string{
		`msg="worktree cleanup completed"`,
		`actor=daemon_recovery`,
		`reason=startup_orphan`,
		`repo_id=repo-1`,
		`run_id=run-1`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("orphan cleanup log missing %q:\n%s", want, got)
		}
	}
}

func captureLifecycleLogs(dst *bytes.Buffer) func() {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(dst, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() {
		slog.SetDefault(old)
	}
}
