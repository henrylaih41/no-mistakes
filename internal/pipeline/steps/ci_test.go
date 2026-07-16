package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIStep_PendingChecksUseAdaptivePollIntervals(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 20 * time.Minute

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started
	var waits []time.Duration

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		now: func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			switch len(waits) {
			case 1:
				current = started.Add(5 * time.Minute)
			case 2:
				current = started.Add(15 * time.Minute)
			case 3:
				cancel()
				return ctx.Err()
			default:
				t.Fatalf("unexpected extra poll wait: %v", interval)
			}
			return nil
		},
	}

	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after observing adaptive waits, got %v", err)
	}

	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("wait count = %d, want %d (%v)", len(waits), len(want), waits)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("wait %d = %v, want %v (all waits: %v)", i, waits[i], want[i], waits)
		}
	}
}

func TestCIStep_UsesStepEnvForCLIStartupChecks(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	hiddenPath := t.TempDir()
	t.Setenv("PATH", hiddenPath)

	env := fakeCIGH(t, "MERGED", "[]")
	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected merged PR to exit cleanly")
	}
	for _, logLine := range logs {
		if strings.Contains(logLine, "gh CLI is not installed") || strings.Contains(logLine, "gh CLI is not authenticated") {
			t.Fatalf("expected startup checks to use StepContext env, got logs: %v", logs)
		}
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "PR has been merged") {
		t.Fatalf("expected CI monitoring to reach PR state check, got logs: %v", logs)
	}
}

func TestCIStep_InvalidPRURLReturnsError(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42/files"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL

	step := &CIStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error for invalid PR URL")
	}
	if !strings.Contains(err.Error(), "extract PR number") {
		t.Fatalf("expected extract PR number context, got %v", err)
	}
	if !strings.Contains(err.Error(), `invalid PR number "files"`) {
		t.Fatalf("expected invalid PR number detail, got %v", err)
	}
}

func TestCIStep_ContextCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	prURL := "https://github.com/test/repo/pull/1"
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	sctx.Ctx = ctx

	step := &CIStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCIStep_Execute_FixMode_RemoteAlreadyUpdatedDoesNotReturnManualIntervention(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	originalHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	os.WriteFile(filepath.Join(dir, "resolved.txt"), []byte("resolved"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "resolve conflict")
	advancedHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "--force-with-lease", "origin", "HEAD:refs/heads/feature")

	checksJSON := `[{"name":"build","state":"FAILURE","bucket":"fail"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "MERGEABLE")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, originalHeadSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Fixing = true
	sctx.Config.CITimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected polling to continue after head reconciliation, got %v", err)
	}

	if sctx.Run.HeadSHA != advancedHeadSHA {
		t.Fatalf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, advancedHeadSHA)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != advancedHeadSHA {
		t.Fatalf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, advancedHeadSHA)
	}
}

func TestCIStep_PRMergedExitsEarly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "MERGED", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for merged PR")
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "merged") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'merged' in logs, got: %v", logs)
	}
}

func TestCIStep_PRClosedExitsEarly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "CLOSED", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for closed PR")
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "closed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'closed' in logs, got: %v", logs)
	}
}

func TestCIStep_GetCIChecksNoChecksReported(t *testing.T) {
	t.Parallel()
	env := fakeCIGHNoChecks(t)

	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})
	sctx.Env = env

	host, skip := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatalf("buildHost returned nil: %s", skip)
	}
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("expected no error when gh reports no checks, got: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected no checks, got: %#v", checks)
	}
}

func TestCIStep_AllChecksPassingKeepsMonitoringOpenPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount == 1 {
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue after passing checks, got %v", err)
	}
	if pollCount != 2 {
		t.Fatalf("expected one pending wait plus one healthy monitoring wait, got %d", pollCount)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected continued-monitoring CI log, got: %v", logs)
	}
}

func TestCIStep_CIWarningAllowsChecksPassedToBeReannounced(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		`not-json`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	waits := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			waits++
			if waits == 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue, got %v", err)
	}

	passedLogs := 0
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed - still monitoring until merged or closed") {
			passedLogs++
		}
	}
	if passedLogs != 2 {
		t.Fatalf("expected checks-passed status before and after CI warning, got %d logs: %v", passedLogs, logs)
	}
}

func TestCIStep_OpenPRKeepsMonitoringAfterChecksPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue after passing checks, got %v", err)
	}
	if pollCount != 1 {
		t.Fatalf("poll count = %d, want 1", pollCount)
	}
}

func TestCIStep_EmptyChecksWaitsDuringGracePeriod(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Fake gh returns OPEN state, empty checks, no comments
	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started
	var waits []time.Duration

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    200 * time.Millisecond,
		pollIntervalOverride: 75 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			if current.Sub(started) >= 200*time.Millisecond {
				cancel()
				return ctx.Err()
			}
			current = current.Add(interval)
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after grace-period monitoring continued, got %v", err)
	}
	if elapsed := current.Sub(started); elapsed < 200*time.Millisecond {
		t.Errorf("CI exited in %v, expected to wait at least 200ms grace period", elapsed)
	}
	if len(waits) != 4 {
		t.Fatalf("expected 3 grace-period waits plus one continued-monitoring wait, got %v", waits)
	}
	for _, interval := range waits[:3] {
		if interval != 75*time.Millisecond {
			t.Fatalf("expected 75ms waits during grace period, got %v", waits)
		}
	}
	for _, l := range logs {
		if strings.Contains(l, "CI timeout reached") {
			t.Fatal("expected cancellation before CI timeout")
		}
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "no CI checks reported - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected continued-monitoring log after grace period, got: %v", logs)
	}
}

func TestCIStep_LogsWaitingForChecksDuringGracePeriod(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	current := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	step := &CIStep{
		checksGracePeriod:    50 * time.Millisecond,
		pollIntervalOverride: 10 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after first grace-period wait, got %v", err)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "waiting for checks to register") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected grace-period waiting log, got: %v", logs)
	}
}

func TestCIStep_NonEmptyPassingChecksSkipGracePeriodAndContinueMonitoring(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		checksGracePeriod: 10 * time.Second,
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue after passing checks, got %v", err)
	}
	if pollCount != 1 {
		t.Fatalf("expected one healthy monitoring wait, got %d", pollCount)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected continued-monitoring pass log, got: %v", logs)
	}
}

// TestCIStep_BaseBranchAdvanceRearmsTimeout verifies the monitor survives past
// its original idle timeout when the base branch advances mid-monitoring: each
// advance re-arms the deadline so a long-held green PR keeps getting watched
// and rebased instead of being silently dropped.
func TestCIStep_BaseBranchAdvanceRearmsTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	tipCalls := 0
	pollCount := 0
	step := &CIStep{
		now: func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) {
			tipCalls++
			if tipCalls == 1 {
				return "sha-old", true
			}
			return "sha-new", true
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			switch pollCount {
			case 1:
				current = started.Add(8 * time.Second)
			case 2:
				// 16s since start is past the 10s timeout, but the base advanced
				// at 8s and re-armed the deadline, so monitoring must continue.
				current = started.Add(16 * time.Second)
			default:
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}

	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue past the original timeout after re-arm, got %v", err)
	}

	rearmed := false
	for _, l := range logs {
		if strings.Contains(l, "re-arming CI monitor timeout") {
			rearmed = true
		}
		if strings.Contains(l, "CI timeout reached") {
			t.Fatalf("monitor timed out despite a base-branch advance re-arm; logs: %v", logs)
		}
	}
	if !rearmed {
		t.Fatalf("expected a re-arm log after the base branch advanced; logs: %v", logs)
	}
}

// TestCIStep_StableBaseStillTimesOut verifies the timeout still fires normally
// for a PR whose base branch never moves, preserving the bounded-monitoring
// behavior for genuinely idle/abandoned PRs.
func TestCIStep_StableBaseStillTimesOut(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		now:           func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) { return "sha-stable", true },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			current = started.Add(12 * time.Second)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected timeout outcome, got error %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected timeout to surface a needs-approval outcome, got %+v", outcome)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "CI timeout reached") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'CI timeout reached' log for a stable base, got: %v", logs)
	}
}

func TestCIStep_UnresolvedFallbackBaseTipDoesNotRearmTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	tipCalls := 0
	pollCount := 0
	step := &CIStep{
		now: func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) {
			tipCalls++
			switch tipCalls {
			case 1:
				return "sha-remote", true
			case 2:
				return baseSHA, false
			default:
				return "sha-remote", true
			}
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			switch pollCount {
			case 1:
				current = started.Add(8 * time.Second)
			case 2:
				current = started.Add(16 * time.Second)
			default:
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected timeout outcome, got error %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected timeout to surface a needs-approval outcome, got %+v", outcome)
	}
	for _, l := range logs {
		if strings.Contains(l, "re-arming CI monitor timeout") {
			t.Fatalf("fallback base SHA must not re-arm timeout; logs: %v", logs)
		}
	}
}

func TestCIStep_ExpiredTimeoutSkipsBaseTipResolver(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	tipCalls := 0
	step := &CIStep{
		now: func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) {
			tipCalls++
			if tipCalls > 1 {
				t.Fatal("base tip resolver should not run after timeout expiry")
			}
			return "sha-stable", true
		},
		waitForNextPoll: func(context.Context, time.Duration) error {
			current = started.Add(11 * time.Second)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected timeout outcome, got error %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected timeout to surface a needs-approval outcome, got %+v", outcome)
	}
	if tipCalls != 1 {
		t.Fatalf("base tip resolver calls = %d, want 1", tipCalls)
	}
}

func TestCIStep_BaseTipResolverDeadlineIsBoundedByRemainingTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	tipCalls := 0
	step := &CIStep{
		now: func() time.Time { return current },
		baseBranchTip: func(ctx context.Context) (string, bool) {
			tipCalls++
			if tipCalls == 1 {
				return "sha-stable", true
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("expected base tip resolver context to have a deadline")
			}
			if remaining := time.Until(deadline); remaining > 2*time.Second {
				t.Fatalf("base tip resolver deadline = %v from now, want no more than 2s", remaining)
			}
			return "sha-stable", true
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			if tipCalls == 1 {
				current = started.Add(8 * time.Second)
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}

	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after deadline inspection, got %v", err)
	}
}

// TestCIStep_UnlimitedTimeoutNeverExpires verifies that an unlimited timeout
// (ci_timeout: "unlimited" / non-positive) makes the monitor watch until the
// PR merges or closes, never self-terminating, and skips base-tip polling.
func TestCIStep_UnlimitedTimeoutNeverExpires(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = config.CITimeoutUnlimited

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	tipCalls := 0
	pollCount := 0
	step := &CIStep{
		now:           func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) { tipCalls++; return "sha", true },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount >= 2 {
				cancel()
				return ctx.Err()
			}
			// Jump far past any finite default timeout to prove it never fires.
			current = started.Add(30 * 24 * time.Hour)
			return nil
		},
	}

	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected unlimited monitoring to continue indefinitely, got %v", err)
	}
	if tipCalls != 0 {
		t.Fatalf("expected no base-tip polling under an unlimited timeout, got %d calls", tipCalls)
	}
	timeoutLog, noTimeoutLog := false, false
	for _, l := range logs {
		if strings.Contains(l, "CI timeout reached") {
			timeoutLog = true
		}
		if strings.Contains(l, "no timeout, until merged or closed") {
			noTimeoutLog = true
		}
	}
	if timeoutLog {
		t.Fatalf("unlimited monitor must not time out; logs: %v", logs)
	}
	if !noTimeoutLog {
		t.Fatalf("expected the no-timeout monitoring log, got: %v", logs)
	}
}

// reviewThreadsEnvelope wraps thread nodes in the GraphQL envelope the production
// parser expects (single page, hasNextPage:false).
func reviewThreadsEnvelope(nodes ...string) string {
	return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` +
		strings.Join(nodes, ",") + `]}}}}}`
}

// reviewThreadNode renders one live, unresolved, non-outdated bot thread with the
// REAL GraphQL login (bare slug, no "[bot]") and __typename "Bot" — the actual
// API shape that exposed the login-normalization bug.
func reviewThreadNode(databaseID int64, path string, line int, body string) string {
	return fmt.Sprintf(
		`{"isResolved":false,"isOutdated":false,"comments":{"nodes":[{"author":{"login":"devin-ai-integration","__typename":"Bot"},"databaseId":%d,"path":%q,"line":%d,"originalLine":%d,"body":%q,"url":"https://github.com/test/repo/pull/42#discussion"}]}}`,
		databaseID, path, line, line, body,
	)
}

// devinReviewsJSON renders the REST pulls/{n}/reviews response: a COMMENTED
// review by the bot (REST-form login with "[bot]" + type "Bot") on the head SHA
// carrying Devin's clean top-level verdict body.
func devinReviewsJSON(headSHA string) string {
	return fmt.Sprintf(`[{"state":"COMMENTED","commit_id":%q,"body":"## ✅ Devin Review: No Issues Found","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]`, headSHA)
}

// devinReviewsJSONWithBody is devinReviewsJSON with a caller-supplied top-level
// review body, for exercising the findings/ambiguous body verdicts.
func devinReviewsJSONWithBody(headSHA, body string) string {
	return fmt.Sprintf(`[{"state":"COMMENTED","commit_id":%q,"body":%q,"user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]`, headSHA, body)
}

// TestCIStep_DevinGreenProceedsToReady is the Bug 2 acceptance test: with no
// GitHub checks and a green Devin verdict (APPROVED, no live severe findings),
// the CI step must proceed to "ready" (ciNoChecksPassedMsg → cimonitor ready)
// after the checks grace period — NOT park on "waiting on Devin review".
//
// On the buggy code (before the login fix), Devin's COMMENTED review with inline
// findings read as APPROVED with 0 findings (false green), but the loop still
// parked because the verdict was PENDING/NONE while waiting for Devin to post.
// With the login fix, a real green (genuinely no findings) must proceed.
func TestCIStep_DevinGreenProceedsToReady(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// No GitHub checks; Devin reviewed the head (COMMENTED) with NO live
	// findings → VerdictApproved → devinDecisionGreen.
	env := fakeCIGHWithReviews(t, "OPEN", "[]",
		devinReviewsJSON(headSHA),
		reviewThreadsEnvelope(), // no threads → 0 findings
	)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second
	sctx.Config.ReviewLoop = config.ReviewLoop{Enabled: true, MaxRounds: 3}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    200 * time.Millisecond,
		pollIntervalOverride: 75 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			if current.Sub(started) >= 400*time.Millisecond {
				cancel()
				return ctx.Err()
			}
			current = current.Add(interval)
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after proceeding to ready, got %v", err)
	}

	// The CI step must have logged the no-checks passed message (ready), NOT
	// parked on "waiting on Devin review".
	foundReady := false
	foundWaiting := false
	for _, l := range logs {
		if strings.Contains(l, cimonitor.NoChecksPassedMsg) {
			foundReady = true
		}
		if strings.Contains(l, cimonitor.WaitingOnReviewMsg) {
			foundWaiting = true
		}
	}
	if !foundReady {
		t.Fatalf("expected %q in logs (Devin green + no checks → ready), got: %v", cimonitor.NoChecksPassedMsg, logs)
	}
	if foundWaiting {
		t.Fatalf("unexpected %q in logs (Devin green must NOT park on waiting), got: %v", cimonitor.WaitingOnReviewMsg, logs)
	}
	// cimonitor must agree the PR is ready.
	if !cimonitor.ChecksPassed(logs) {
		t.Fatalf("cimonitor.ChecksPassed(logs) = false, want true (Devin green + no checks → ready): %v", logs)
	}
}

// TestCIStep_DevinSevereFindingsDriveFix is the Bug 1+2 integration test: with
// no GitHub checks and Devin's COMMENTED review carrying a live severe finding
// (read via the real graphql read layer with the real login), the CI step must
// drive a fix round (log ReviewChangesRequestedMsg + auto-fixing) — NOT read 0
// findings (Bug 1) and NOT park on a false green (Bug 2).
func TestCIStep_DevinSevereFindingsDriveFix(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// No GitHub checks; Devin reviewed the head (COMMENTED) with ONE live 🔴
	// high-severity finding → VerdictChangesRequested → devinDecisionNotGreen.
	threads := reviewThreadsEnvelope(
		reviewThreadNode(123, "internal/batch/download.go", 42, "🔴 **Batch download crashes on empty manifest**"),
	)
	env := fakeCIGHWithReviews(t, "OPEN", "[]",
		devinReviewsJSON(headSHA),
		threads,
	)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second
	sctx.Config.ReviewLoop = config.ReviewLoop{Enabled: true, MaxRounds: 3, ReplyOnFix: true}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    50 * time.Millisecond,
		pollIntervalOverride: 50 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			// Let the loop run long enough to drive at least one fix round.
			// The mock agent produces no changes, so the loop will exhaust its
			// rounds and escalate; we cancel shortly after to bound runtime.
			if current.Sub(started) >= 250*time.Millisecond {
				cancel()
				return ctx.Err()
			}
			current = current.Add(interval)
			return nil
		},
	}
	step.Execute(sctx) // outcome/err varies (escalation or cancel); we assert on logs.

	// The CI step must have logged that Devin requested changes AND started
	// auto-fixing — proving the loop read the finding (Bug 1 fixed) and drove
	// a fix (not parked on a false green, Bug 2 fixed).
	foundChangesRequested := false
	foundAutoFixing := false
	for _, l := range logs {
		if strings.Contains(l, cimonitor.ReviewChangesRequestedMsg) {
			foundChangesRequested = true
		}
		if strings.Contains(l, "auto-fixing") {
			foundAutoFixing = true
		}
	}
	if !foundChangesRequested {
		t.Fatalf("expected %q in logs (Devin severe finding → changes requested), got: %v", cimonitor.ReviewChangesRequestedMsg, logs)
	}
	if !foundAutoFixing {
		t.Fatalf("expected auto-fixing in logs (Devin severe finding → fix round), got: %v", logs)
	}
	// Must NOT have declared ready while severe findings are live.
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("cimonitor.ChecksPassed = true, want false (severe findings live → not ready): %v", logs)
	}
}

// TestCIStep_DevinNeverReviewedStillWaits verifies the fix didn't break the
// legitimate wait-for-review case: when Devin has never reviewed the PR at all
// (no REST reviews), the loop must park on "waiting on Devin review" (not
// declare ready). This guards against the fix over-proceeding.
func TestCIStep_DevinNeverReviewedStillWaits(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// No GitHub checks; Devin has NOT reviewed (empty REST reviews, empty
	// threads) → VerdictNone → devinDecisionPending → park.
	env := fakeCIGHWithReviews(t, "OPEN", "[]", "[]", reviewThreadsEnvelope())

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second
	sctx.Config.ReviewLoop = config.ReviewLoop{Enabled: true, MaxRounds: 3}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    50 * time.Millisecond,
		pollIntervalOverride: 50 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			if current.Sub(started) >= 200*time.Millisecond {
				cancel()
				return ctx.Err()
			}
			current = current.Add(interval)
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation while waiting, got %v", err)
	}

	// Must have logged waiting-on-review (Devin never reviewed → legitimately
	// pending) and NOT declared ready.
	foundWaiting := false
	for _, l := range logs {
		if strings.Contains(l, cimonitor.WaitingOnReviewMsg) {
			foundWaiting = true
		}
	}
	if !foundWaiting {
		t.Fatalf("expected %q in logs (Devin never reviewed → wait), got: %v", cimonitor.WaitingOnReviewMsg, logs)
	}
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("cimonitor.ChecksPassed = true, want false (Devin never reviewed → not ready): %v", logs)
	}
}

// TestCIStep_DevinFindingsBodyNoThreadsNeedsManualReview is the nm#20 /
// review-claude-1-1 acceptance test: Devin's COMMENTED review body reports
// findings on the current head, but no file-scoped threads loaded. The loop must
// NOT run the auto-fixer (which would fabricate changes for a problem it cannot
// see, ruling #11) — once the grace window elapses it must park at the human gate
// with the explicit "manual verify" reason.
func TestCIStep_DevinFindingsBodyNoThreadsNeedsManualReview(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGHWithReviews(t, "OPEN", "[]",
		devinReviewsJSONWithBody(headSHA, "## ⚠️ Devin Review: Found 2 potential issues"),
		reviewThreadsEnvelope(), // body says found N, but no threads load
	)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("auto-fixer must not run when Devin's findings could not be loaded")
		return nil, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour
	sctx.Config.ReviewLoop = config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    time.Minute,
		pollIntervalOverride: 5 * time.Minute, // jump past the 11m Devin grace window
		now:                  func() time.Time { return current },
		baseBranchTip:        func(context.Context) (string, bool) { return "basetip", true },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			if current.Sub(started) >= time.Hour {
				cancel()
				return ctx.Err()
			}
			current = current.Add(interval)
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked outcome", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected a NeedsApproval park for manual review, got %+v", outcome)
	}
	if !strings.Contains(outcome.Findings, "manual verify") {
		t.Fatalf("park findings must carry the manual-verify reason, got %q", outcome.Findings)
	}
	foundManual := false
	for _, l := range logs {
		if strings.Contains(l, cimonitor.ReviewManualVerifyMsg) {
			foundManual = true
		}
	}
	if !foundManual {
		t.Fatalf("expected %q in logs, got: %v", cimonitor.ReviewManualVerifyMsg, logs)
	}
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("cimonitor.ChecksPassed = true, want false (manual review needed → not ready): %v", logs)
	}
}

// TestCIStep_FailingCheckWithDevinManualReviewSurfacesBothAtTimeout is the
// review-codex-2-1 acceptance test: when a CI check is failing AND Devin reported
// a body-only not-green signal on the current head (findings, no loadable
// threads), the CI-failure park must NOT hide the manual-verify signal (ruling
// #3). A failing check plus a still-pending check keeps the monitor in the
// "waiting on checks" branch until the CI timeout fires, at which point the
// combined outcome must carry BOTH the failing check and the manual-verify
// finding.
func TestCIStep_FailingCheckWithDevinManualReviewSurfacesBothAtTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"FAILURE","bucket":"fail"},{"name":"deploy","state":"PENDING","bucket":"pending"}]`
	env := fakeCIGHWithReviews(t, "OPEN", checksJSON,
		devinReviewsJSONWithBody(headSHA, "## ⚠️ Devin Review: Found 2 potential issues"),
		reviewThreadsEnvelope(), // body says found N, but no threads load -> manual verify
	)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("auto-fixer must not run while a check is still pending")
		return nil, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.ReviewLoop = config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    time.Minute,
		pollIntervalOverride: 5 * time.Minute, // step past the 11m Devin grace window before timeout
		now:                  func() time.Time { return current },
		baseBranchTip:        func(context.Context) (string, bool) { return "basetip", true },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			current = current.Add(interval)
			if current.Sub(started) > 2*time.Hour { // safety net against a stuck loop
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked timeout outcome", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected a NeedsApproval park, got %+v", outcome)
	}
	if !strings.Contains(outcome.Findings, "build") {
		t.Errorf("park findings must carry the failing check, got %q", outcome.Findings)
	}
	if !strings.Contains(outcome.Findings, cimonitor.ReviewManualVerifyMsg) {
		t.Errorf("park findings must also carry the Devin manual-verify signal, got %q", outcome.Findings)
	}
}

// TestCIStep_FailingCheckWithDevinManualReviewPendingWithinGraceSurfacesBoth is
// the review-codex-2-1 within-grace regression test. Before the fix, a
// MANUAL_REVIEW verdict (body-only not-green: "found N potential issues", no
// loadable threads) was downgraded to a plain pending during the 11-minute
// grace window, so it carried NO manual-verify reason. If a CI gate parked in
// the same poll before the window elapsed — here a failing check with auto-fix
// disabled — the body-only not-green signal disappeared behind the CI-only park
// (ruling #3 violation). The fix keeps a distinct manual-review-pending signal
// during grace and folds it into the earlier CI gate, so the park must carry
// BOTH the failing check AND the awaiting-manual-verify reason.
func TestCIStep_FailingCheckWithDevinManualReviewPendingWithinGraceSurfacesBoth(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// A failing check with NO pending check: all checks are done this poll, so the
	// monitor decides to park immediately rather than wait for pending checks.
	checksJSON := `[{"name":"build","state":"FAILURE","bucket":"fail"}]`
	env := fakeCIGHWithReviews(t, "OPEN", checksJSON,
		devinReviewsJSONWithBody(headSHA, "## ⚠️ Devin Review: Found 2 potential issues"),
		reviewThreadsEnvelope(), // body says found N, but no threads load -> manual verify
	)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("auto-fixer must not run when CI auto-fix is disabled")
		return nil, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour
	sctx.Config.AutoFix = config.AutoFix{CI: 0} // disabled -> park immediately, still within grace
	sctx.Config.ReviewLoop = config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    time.Minute,
		pollIntervalOverride: time.Second, // stay well within the 11m Devin grace window
		now:                  func() time.Time { return current },
		baseBranchTip:        func(context.Context) (string, bool) { return "basetip", true },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			// The disabled-auto-fix park returns on the first poll; if we ever loop,
			// bail out rather than spin so a regression surfaces as a failure.
			cancel()
			return ctx.Err()
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked outcome within grace", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected a NeedsApproval park, got %+v", outcome)
	}
	if !strings.Contains(outcome.Findings, "build") {
		t.Errorf("park findings must carry the failing check, got %q", outcome.Findings)
	}
	if !strings.Contains(outcome.Findings, cimonitor.ReviewManualVerifyPendingMsg) {
		t.Errorf("park findings must also carry the pending Devin manual-verify signal (never hidden during grace), got %q", outcome.Findings)
	}
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("cimonitor.ChecksPassed = true, want false (manual review pending -> not ready): %v", logs)
	}
}

// TestCIStep_DevinAmbiguousBodyLogsExplicitReason is the review-codex-2-1
// acceptance test: a current-head COMMENTED review whose body carries no
// recognizable verdict must surface an explicit "ambiguous body" reason rather
// than the generic "waiting on Devin review" (which conflates it with "the bot
// has not reviewed this head yet").
func TestCIStep_DevinAmbiguousBodyLogsExplicitReason(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGHWithReviews(t, "OPEN", "[]",
		devinReviewsJSONWithBody(headSHA, "## Devin Review\nI looked at this change."),
		reviewThreadsEnvelope(), // no threads; body is unrecognized
	)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second
	// Fail-closed so the ambiguous state keeps its distinct pending reason rather
	// than fail-opening to green while we assert on the log.
	sctx.Config.ReviewLoop = config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: false}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    50 * time.Millisecond,
		pollIntervalOverride: 50 * time.Millisecond,
		now:                  func() time.Time { return current },
		baseBranchTip:        func(context.Context) (string, bool) { return "basetip", true },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			if current.Sub(started) >= 300*time.Millisecond {
				cancel()
				return ctx.Err()
			}
			current = current.Add(interval)
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation while waiting on an ambiguous body, got %v", err)
	}

	foundAmbiguous := false
	foundWaiting := false
	for _, l := range logs {
		if strings.Contains(l, cimonitor.ReviewBodyAmbiguousMsg) {
			foundAmbiguous = true
		}
		if strings.Contains(l, cimonitor.WaitingOnReviewMsg) {
			foundWaiting = true
		}
	}
	if !foundAmbiguous {
		t.Fatalf("expected %q in logs (ambiguous body → explicit reason), got: %v", cimonitor.ReviewBodyAmbiguousMsg, logs)
	}
	if foundWaiting {
		t.Fatalf("ambiguous body must not be logged as generic %q, got: %v", cimonitor.WaitingOnReviewMsg, logs)
	}
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("cimonitor.ChecksPassed = true, want false (ambiguous body → not ready): %v", logs)
	}
}
