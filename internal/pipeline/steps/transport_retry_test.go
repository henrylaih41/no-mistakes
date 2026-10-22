package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

// fastTransportRetry shrinks the backoff to milliseconds. The tests that call
// it are serial on purpose: the policy is package state.
func fastTransportRetry(t *testing.T, attemptTimeout time.Duration) {
	t.Helper()
	oldBackoff, oldTimeout := transportBackoff, transportAttemptTimeout
	transportBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	transportAttemptTimeout = attemptTimeout
	t.Cleanup(func() { transportBackoff, transportAttemptTimeout = oldBackoff, oldTimeout })
}

// captureLog replaces sctx.Log with a collector and returns a reader for it.
func captureLog(sctx *pipeline.StepContext) func() string {
	var mu sync.Mutex
	var lines []string
	sctx.Log = func(s string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, s)
	}
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

func TestIsTransportError(t *testing.T) {
	retry := []string{
		"git push x: exit status 1: fatal: the remote end hung up unexpectedly",
		"send-pack: unexpected disconnect while reading sideband packet",
		"remote: error: inflate: data stream error (incorrect data check)\nerror: remote unpack failed: index-pack failed",
		"fatal: early EOF",
		"read tcp: connection reset by peer",
		"error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400",
		"gh pr create: HTTP 504: 504 Gateway Timeout (https://api.github.com/graphql)",
		"gh pr create: non-200 OK status code: 502 Bad Gateway body: x",
		"dial tcp: i/o timeout",
		"Bitbucket POST pull request: status 503: service unavailable",
		"fatal: unable to access x: The requested URL returned error: 503",
	}
	for _, msg := range retry {
		if !isTransportError(errors.New(msg)) {
			t.Errorf("want transport error: %q", msg)
		}
	}
	fail := []string{
		"! [rejected] feature -> feature (non-fast-forward)",
		"! [rejected] feature -> feature (stale info)",
		"! [remote rejected] feature -> feature (pre-receive hook declined)",
		"fatal: Authentication failed for 'https://github.com/x/y.git/'",
		"git@github.com: Permission denied (publickey).",
		"error: RPC failed; HTTP 403 curl 22 The requested URL returned error: 403",
		"gh pr create: HTTP 422: Validation Failed",
		"gh pr create: HTTP 404: Not Found",
		"gh pr create: HTTP 429: rate limited, connection reset",
		"Bitbucket GET x: status 400: bad request, early EOF",
		"tea: returned error: 409 conflict, i/o timeout",
		"fatal: unable to access x: The requested URL returned error: 403, connection reset",
		"",
	}
	for _, msg := range fail {
		if isTransportError(errors.New(msg)) {
			t.Errorf("want non-transport error: %q", msg)
		}
	}
	if isTransportError(nil) {
		t.Error("nil is not a transport error")
	}
}

func TestRetryTransport_RetriesThenSucceeds(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	logs := captureLog(sctx)
	calls := 0
	err := retryTransport(sctx, "push", func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("fatal: the remote end hung up unexpectedly")
		}
		return nil
	}, nil)
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want success on call 3", err, calls)
	}
	for _, want := range []string{"push: transport error (attempt 1/4)", "push: transport error (attempt 2/4)"} {
		if !strings.Contains(logs(), want) {
			t.Errorf("log missing %q:\n%s", want, logs())
		}
	}
}

func TestRetryTransport_GivesUpAfterTheLastBackoff(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	captureLog(sctx)
	calls := 0
	err := retryTransport(sctx, "push", func(context.Context) error {
		calls++
		return errors.New("fatal: the remote end hung up unexpectedly")
	}, nil)
	if err == nil || calls != 4 {
		t.Fatalf("err=%v calls=%d, want 4 attempts then the error", err, calls)
	}
}

func TestRetryTransport_NonTransportFailsAtOnce(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	captureLog(sctx)
	calls := 0
	err := retryTransport(sctx, "push", func(context.Context) error {
		calls++
		return errors.New("! [rejected] feature -> feature (non-fast-forward)")
	}, func(context.Context) (bool, error) {
		t.Error("landed must not be asked for a refusal")
		return false, nil
	})
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want one attempt", err, calls)
	}
}

func TestRetryTransport_TimeoutIsATransportError(t *testing.T) {
	fastTransportRetry(t, 20*time.Millisecond)
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	logs := captureLog(sctx)
	calls := 0
	err := retryTransport(sctx, "push", func(ctx context.Context) error {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return errors.New("signal: killed")
		}
		return nil
	}, nil)
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want a retry after the timeout", err, calls)
	}
	if !strings.Contains(logs(), "timed out after") {
		t.Errorf("log should name the timeout:\n%s", logs())
	}
}

func TestRetryTransport_LandedStopsTheRetry(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	captureLog(sctx)
	calls := 0
	err := retryTransport(sctx, "push", func(context.Context) error {
		calls++
		return errors.New("fatal: the remote end hung up unexpectedly")
	}, func(context.Context) (bool, error) { return true, nil })
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want success without a second attempt", err, calls)
	}
}

func TestRetryTransport_UnknownLandedNeverRunsAnotherAttempt(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	captureLog(sctx)
	calls, lookups := 0, 0
	err := retryTransport(sctx, "push", func(context.Context) error {
		calls++
		return errors.New("fatal: the remote end hung up unexpectedly")
	}, func(context.Context) (bool, error) {
		lookups++
		return false, errors.New("ls-remote: connection reset")
	})
	if err == nil || !strings.Contains(err.Error(), "hung up") {
		t.Fatalf("err=%v, want the original attempt error", err)
	}
	if calls != 1 || lookups != 3 {
		t.Fatalf("calls=%d lookups=%d, want 1 attempt and a lookup per backoff slot", calls, lookups)
	}
}

func TestRetryTransport_LandedLookupFailingOnceThenLandedRunsOneAttempt(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	captureLog(sctx)
	calls, lookups := 0, 0
	err := retryTransport(sctx, "push", func(context.Context) error {
		calls++
		return errors.New("fatal: the remote end hung up unexpectedly")
	}, func(context.Context) (bool, error) {
		lookups++
		if lookups == 1 {
			return false, errors.New("ls-remote: connection reset")
		}
		return true, nil
	})
	if err != nil || calls != 1 || lookups != 2 {
		t.Fatalf("err=%v calls=%d lookups=%d, want success after one attempt", err, calls, lookups)
	}
}

func TestRetryTransport_CancelledRunStopsAtOnce(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	sctx := &pipeline.StepContext{Ctx: ctx}
	captureLog(sctx)
	calls := 0
	err := retryTransport(sctx, "push", func(context.Context) error {
		calls++
		cancel()
		return errors.New("fatal: the remote end hung up unexpectedly")
	}, nil)
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want the first error and no retry", err, calls)
	}
}

// pushRetryFixture is a feature branch one commit ahead of its remote, with
// the step's git replaced by the flaky-push fake. The returned log file counts
// the pushes the step made.
func pushRetryFixture(t *testing.T, vars map[string]string, commitBytes ...int) (sctx *pipeline.StepContext, upstream, head, logFile string) {
	t.Helper()
	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")
	if len(commitBytes) == 0 {
		commitBytes = []int{len("next\n")}
	}
	for i, n := range commitBytes {
		commitRandom(t, dir, fmt.Sprintf("next%d.bin", i), fmt.Sprintf("next %d", i), n)
	}
	head = gitCmd(t, dir, "rev-parse", "HEAD")

	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	logFile = filepath.Join(t.TempDir(), "git.log")
	env := map[string]string{"FAKE_CLI_MODE": "git-flaky-push-passthrough", "FAKE_CLI_LOG": logFile, "FAKE_CLI_REAL_GIT": realGit}
	for k, v := range vars {
		env[k] = v
	}

	sctx = newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Env = fakeCLIEnv(binDir, env)
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, head)
	return sctx, upstream, head, logFile
}

func countPushes(t *testing.T, logFile string) int {
	t.Helper()
	return fakeLogCount(t, logFile, "push ")
}

func fakeLogCount(t *testing.T, logFile, prefix string) int {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	return countLines(string(data), prefix)
}

func TestPushStep_RetriesDroppedPushesUntilOneLands(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx, upstream, head, logFile := pushRetryFixture(t, map[string]string{"FAKE_CLI_FAIL_FIRST": "2"})
	logs := captureLog(sctx)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got := countPushes(t, logFile); got != 3 {
		t.Errorf("pushes = %d, want 3 (two dropped, one landed)", got)
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != head {
		t.Errorf("remote = %s, want %s", remote, head)
	}
	for _, want := range []string{"push: transport error (attempt 1/4)", "push: transport error (attempt 2/4)"} {
		if !strings.Contains(logs(), want) {
			t.Errorf("step log missing %q:\n%s", want, logs())
		}
	}
}

func TestPushStep_PushThatLandedBeforeTheErrorIsNotRepeated(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx, upstream, head, logFile := pushRetryFixture(t, map[string]string{"FAKE_CLI_FAIL_FIRST": "1", "FAKE_CLI_FAIL_LANDED": "1"})
	logs := captureLog(sctx)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got := countPushes(t, logFile); got != 1 {
		t.Errorf("pushes = %d, want 1 (the landed push must not be repeated)", got)
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != head {
		t.Errorf("remote = %s, want %s", remote, head)
	}
	if !strings.Contains(logs(), "had already landed") {
		t.Errorf("step log should say the push had landed:\n%s", logs())
	}
}

func TestPushStep_RejectedPushIsNotRetried(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx, upstream, _, logFile := pushRetryFixture(t, map[string]string{
		"FAKE_CLI_FAIL_FIRST": "9",
		"FAKE_CLI_PUSH_ERR":   "! [rejected] feature -> feature (non-fast-forward)",
	})
	before := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")

	if _, err := (&PushStep{}).Execute(sctx); err == nil {
		t.Fatal("a rejected push must fail the step")
	}
	if got := countPushes(t, logFile); got != 1 {
		t.Errorf("pushes = %d, want 1 (a rejection fails at once)", got)
	}
	if after := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); after != before {
		t.Errorf("remote moved from %s to %s", before, after)
	}
}

func TestPushStep_HungPushIsKilledAndRetried(t *testing.T) {
	fastTransportRetry(t, 3*time.Second)
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	sctx, upstream, head, logFile := pushRetryFixture(t, map[string]string{"FAKE_CLI_FAIL_FIRST": "1", "FAKE_CLI_FAIL_HANG": "1", "FAKE_CLI_HEARTBEAT_FILE": heartbeat})
	logs := captureLog(sctx)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(heartbeat)
	if err != nil || before.Size() == 0 {
		t.Fatalf("the hung push's child never ran: stat=%v err=%v", before, err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := os.Stat(heartbeat)
	if err != nil || after.Size() != before.Size() {
		t.Errorf("the hung push's child outlived its attempt: heartbeat grew from %d to %d bytes", before.Size(), after.Size())
	}
	if got := countPushes(t, logFile); got != 2 {
		t.Errorf("pushes = %d, want 2 (one hung, one landed)", got)
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != head {
		t.Errorf("remote = %s, want %s", remote, head)
	}
	if !strings.Contains(logs(), "timed out after 3s") {
		t.Errorf("step log should name the timeout:\n%s", logs())
	}
}

func TestPushStep_FailsAfterTheRetriesAreSpent(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	sctx, _, _, logFile := pushRetryFixture(t, map[string]string{"FAKE_CLI_FAIL_FIRST": "99"})

	if _, err := (&PushStep{}).Execute(sctx); err == nil {
		t.Fatal("a push that never lands must fail the step")
	}
	if got := countPushes(t, logFile); got != 4 {
		t.Errorf("pushes = %d, want 4 (first try plus three retries)", got)
	}
}

// prRetryStep runs the PR step against a fake gh configured by vars and
// returns the step error, the gh log and the stored PR URL.
func prRetryStep(t *testing.T, vars map[string]string) (error, string, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ghEnv, logFile := fakeGH(t, "")
	ghEnv = append(ghEnv, "FAKE_CLI_PR_CREATED_FILE="+filepath.Join(t.TempDir(), "created-pr"))
	for k, v := range vars {
		ghEnv = append(ghEnv, k+"="+v)
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = ghEnv
	captureLog(sctx)

	_, err := (&PRStep{}).Execute(sctx)
	run, getErr := sctx.DB.GetRun(sctx.Run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	url := ""
	if run.PRURL != nil {
		url = *run.PRURL
	}
	data, _ := os.ReadFile(logFile)
	return err, string(data), url
}

func countLines(log, prefix string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

func TestPRStep_RetriesACreateThatGotA504(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	err, ghLog, url := prRetryStep(t, map[string]string{
		"FAKE_CLI_PR_CREATE_FAIL_FIRST": "1",
		"FAKE_CLI_PR_CREATE_ERR":        "HTTP 504: 504 Gateway Timeout (https://api.github.com/graphql)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countLines(ghLog, "pr create"); got != 2 {
		t.Errorf("creates = %d, want 2 (one 504, one success)", got)
	}
	if url != "https://github.com/test/repo/pull/99" {
		t.Errorf("PR URL = %q", url)
	}
}

func TestPRStep_RetriesAHostProbeThatGotA503(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	err, ghLog, url := prRetryStep(t, map[string]string{
		"FAKE_CLI_AUTH_FAIL_FIRST": "1",
		"FAKE_CLI_AUTH_ERR":        "HTTP 503: Service Unavailable (https://api.github.com/)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countLines(ghLog, "auth status"); got != 2 {
		t.Errorf("probes = %d, want 2 (one 503, one success)", got)
	}
	if got := countLines(ghLog, "pr create"); got != 1 || url != "https://github.com/test/repo/pull/99" {
		t.Errorf("creates = %d, PR URL = %q; want the PR published after the probe recovered", got, url)
	}
}

func TestPRStep_ACreateThatLandedBeforeTheErrorOpensNoSecondPR(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	err, ghLog, url := prRetryStep(t, map[string]string{
		"FAKE_CLI_PR_CREATE_FAIL_FIRST": "1",
		"FAKE_CLI_PR_CREATE_LANDS":      "1",
		"FAKE_CLI_PR_CREATE_ERR":        "HTTP 504: 504 Gateway Timeout (https://api.github.com/graphql)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countLines(ghLog, "pr create"); got != 1 {
		t.Errorf("creates = %d, want 1 (the PR already existed after the 504)", got)
	}
	if url != "https://github.com/test/repo/pull/99" {
		t.Errorf("PR URL = %q, want the PR the failed create opened", url)
	}
}

func TestPRStep_AValidationErrorFailsAtOnce(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	err, ghLog, _ := prRetryStep(t, map[string]string{
		"FAKE_CLI_PR_CREATE_FAIL_FIRST": "9",
		"FAKE_CLI_PR_CREATE_ERR":        "HTTP 422: Validation Failed",
	})
	if err == nil {
		t.Fatal("a 422 must fail the step")
	}
	if got := countLines(ghLog, "pr create"); got != 1 {
		t.Errorf("creates = %d, want 1", got)
	}
}

func TestPushStep_RetriesTheRemoteReadsAroundThePush(t *testing.T) {
	for _, tc := range []struct {
		name string
		nth  string // which ls-remote the fake drops
		step string
	}{
		{"read before the push", "1", "read remote branch"},
		{"verification after the push", "2", "verify push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastTransportRetry(t, time.Minute)
			sctx, upstream, head, logFile := pushRetryFixture(t, map[string]string{"FAKE_CLI_LSREMOTE_FAIL_NTH": tc.nth})
			logs := captureLog(sctx)

			if _, err := (&PushStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if got := fakeLogCount(t, logFile, "ls-remote "); got != 3 {
				t.Errorf("ls-remote calls = %d, want 3 (two reads and one retry)", got)
			}
			if got := countPushes(t, logFile); got != 1 {
				t.Errorf("pushes = %d, want 1", got)
			}
			if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != head {
				t.Errorf("remote = %s, want %s", remote, head)
			}
			if want := tc.step + ": transport error (attempt 1/4)"; !strings.Contains(logs(), want) {
				t.Errorf("step log missing %q:\n%s", want, logs())
			}
		})
	}
}

func TestPRStep_RetriesAnExistingPRContentReadThatGotA504(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	err, ghLog, url := prRetryStep(t, map[string]string{
		"FAKE_CLI_PR_URL":                "https://github.com/test/repo/pull/7",
		"FAKE_CLI_PR_CONTENT_FAIL_FIRST": "1",
		"FAKE_CLI_PR_CONTENT_ERR":        "HTTP 504: 504 Gateway Timeout (https://api.github.com/graphql)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(ghLog, "--json title,body"); got != 2 {
		t.Errorf("content reads = %d, want 2 (one 504, one success)", got)
	}
	if got := countLines(ghLog, "pr edit"); got != 1 {
		t.Errorf("updates = %d, want 1", got)
	}
	if url != "https://github.com/test/repo/pull/7" {
		t.Errorf("PR URL = %q", url)
	}
}

func TestRetryTransport_NilAfterTheDeadlineIsRetried(t *testing.T) {
	fastTransportRetry(t, 20*time.Millisecond)
	sctx := &pipeline.StepContext{Ctx: context.Background(), Log: func(string) {}}
	calls := 0
	err := retryTransport(sctx, "x", func(ctx context.Context) error {
		calls++
		if calls == 1 {
			<-ctx.Done()
		}
		return nil
	}, nil)
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want the expired nil attempt retried once", err, calls)
	}
}
