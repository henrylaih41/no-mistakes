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
// notification was lost). A caller whose clean HEAD is the gate head starts
// the first run; anything else keeps the replay-only error.
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

	for name, callerHead := range map[string]string{
		"no caller head":         "",
		"mismatched caller head": strings.Repeat("d", 40),
	} {
		var result ipc.RerunResult
		if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: "bootstrap-repo", Branch: "main", CallerHeadSHA: callerHead}, &result); err == nil || !strings.Contains(err.Error(), "no previous run") {
			t.Fatalf("%s: err = %v, want 'no previous run'", name, err)
		}
	}

	var boot ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID: "bootstrap-repo", Branch: "main", CallerHeadSHA: headSHA, Intent: "ship the thing",
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
