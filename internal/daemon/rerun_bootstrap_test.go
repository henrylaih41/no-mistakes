package daemon

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The gate holds the branch but no run was ever recorded (a push whose hook
// notification was lost). `axi run`'s fallback, whose clean HEAD is the gate
// head, starts the first run; anything else, including a plain rerun on that
// exact head, keeps the replay-only error.
func TestRerunBootstrapsTheFirstRunOnlyForTheGateHead(t *testing.T) {
	review := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{review}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "bootstrap-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for name, params := range map[string]ipc.RerunParams{
		"no caller head":         {BootstrapFirstRun: true},
		"mismatched caller head": {BootstrapFirstRun: true, CallerHeadSHA: strings.Repeat("d", 40)},
		"plain rerun":            {CallerHeadSHA: headSHA},
	} {
		params.RepoID, params.Branch = "bootstrap-repo", "main"
		var result ipc.RerunResult
		if err := client.Call(ipc.MethodRerun, &params, &result); err == nil || !strings.Contains(err.Error(), "no previous run") {
			t.Fatalf("%s: err = %v, want 'no previous run'", name, err)
		}
	}

	var boot ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID: "bootstrap-repo", Branch: "main", CallerHeadSHA: headSHA, BootstrapFirstRun: true, Intent: "ship the thing",
	}, &boot); err != nil {
		t.Fatalf("bootstrap rerun: %v", err)
	}
	if boot.RunID == "" {
		t.Fatal("bootstrap produced no run")
	}
	run := waitForRunTerminalState(t, d, boot.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("bootstrap run status = %s, want completed", run.Status)
	}
	if !git.IsZeroSHA(run.BaseSHA) || run.HeadSHA != headSHA {
		t.Fatalf("bootstrap run base=%q head=%q, want zero base and head %q", run.BaseSHA, run.HeadSHA, headSHA)
	}
	if run.Intent == nil || *run.Intent != "ship the thing" {
		t.Fatalf("bootstrap intent = %v, want %q", run.Intent, "ship the thing")
	}
	if got := review.execCnt.Load(); got != 1 {
		t.Fatalf("review executed %d times, want 1", got)
	}
}
