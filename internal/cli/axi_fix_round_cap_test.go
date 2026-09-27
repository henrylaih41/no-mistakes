package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func fixRoundCapGateFindings(t *testing.T) string {
	t.Helper()
	return findingsJSON(t, []types.Finding{
		{ID: "review-1", Severity: types.FindingSeverityWarning, File: "main.go", Action: types.ActionAutoFix, Description: "missing error check"},
		{ID: pipeline.FixRoundCapFindingID, Severity: types.FindingSeverityError, Action: types.ActionAskUser, Description: "review.max_fix_rounds (3) is reached"},
	}, "cap reached")
}

func TestAxiRespond_FixOverrideFlagsAreValidatedTogether(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"override without fix", []string{"--action", "approve", "--fix-override", "--override-reason", "x"}, "--fix-override applies only to --action fix"},
		{"override without reason", []string{"--action", "fix", "--findings", "review-1", "--fix-override"}, "--fix-override requires a non-empty --override-reason"},
		{"blank reason", []string{"--action", "fix", "--findings", "review-1", "--fix-override", "--override-reason", "  "}, "--fix-override requires a non-empty --override-reason"},
		{"reason without override", []string{"--action", "fix", "--findings", "review-1", "--override-reason", "x"}, "--override-reason requires --fix-override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := executeCmd(append([]string{"axi", "respond"}, tc.args...)...)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("respond %v = %v, output %q; want a usage error containing %q", tc.args, err, out, tc.want)
			}
		})
	}
}

func TestFixRoundCapGateHelpNamesTheOverride(t *testing.T) {
	got := axiDoc(gateFields(stepView{
		Name:         string(types.StepReview),
		Status:       string(types.StepStatusAwaitingApproval),
		FindingsJSON: fixRoundCapGateFindings(t),
	})...)
	for _, want := range []string{pipeline.FixRoundCapFindingID, "--fix-override --override-reason"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cap gate output is missing %q:\n%s", want, got)
		}
	}
}

func TestDriveRun_YesLeavesAFixRoundCapGateAwaitingADecision(t *testing.T) {
	socketPath := filepath.Join(makeSocketSafeTempDir(t), "fix-round-cap.sock")
	srv := ipc.NewServer()
	var responses atomic.Int32
	srv.Handle(ipc.MethodRespond, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		responses.Add(1)
		return nil, errors.New("unexpected automatic response to a fix-round cap gate")
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(socketPath) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("IPC server did not stop")
		}
	})
	var client *ipc.Client
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		client, err = ipc.Dial(socketPath)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		t.Fatal("IPC server did not become ready")
	}
	defer client.Close()

	findings := fixRoundCapGateFindings(t)
	parked := &ipc.RunInfo{
		ID: "run-1", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings}},
	}
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: make(chan ipc.Event)}},
		runs:          []*ipc.RunInfo{parked},
	}
	reconciler := newRunReconciler(source, parked.ID)
	defer reconciler.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var progress bytes.Buffer
	run, ciReady, err := driveRunWithReconciler(ctx, &progress, client, reconciler, parked.ID, true)
	if err != nil || run != parked || ciReady || responses.Load() != 0 {
		t.Fatalf("--yes resolved a fix-round cap gate: run=%+v ciReady=%v responses=%d err=%v", run, ciReady, responses.Load(), err)
	}
	if !strings.Contains(progress.String(), "review.max_fix_rounds") {
		t.Fatalf("progress does not name the cause: %s", progress.String())
	}
}
