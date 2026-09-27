package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPushReceivedMaterializesDesignContext(t *testing.T) {
	assertPushReceivedMaterializesDesignContext(t, false)
}

func TestPushReceivedMaterializesGlobalDesignContext(t *testing.T) {
	assertPushReceivedMaterializesDesignContext(t, true)
}

func assertPushReceivedMaterializesDesignContext(t *testing.T, global bool) {
	t.Helper()
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	contextPath := filepath.Join(t.TempDir(), "contract.md")
	if err := os.WriteFile(contextPath, []byte("ship the agreed contract"), 0o644); err != nil {
		t.Fatal(err)
	}
	var designContextPaths []string
	if global {
		globalConfig, err := os.ReadFile(p.ConfigFile())
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		globalConfig = append(globalConfig, []byte(fmt.Sprintf("\ndesign_context:\n  files:\n    - %q\n", contextPath))...)
		if err := os.WriteFile(p.ConfigFile(), globalConfig, 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		designContextPaths = []string{contextPath}
	}

	_, headSHA := setupTestGitRepo(t, p, d, "design-context-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:               p.RepoDir("design-context-repo"),
		Ref:                "refs/heads/main",
		Old:                "0000000000000000000000000000000000000000",
		New:                headSHA,
		DesignContextPaths: designContextPaths,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.DesignContextJSON == nil {
		t.Fatal("expected design context JSON to be persisted on run")
	}
	designCtx, err := types.ParseDesignContextJSON(*run.DesignContextJSON)
	if err != nil {
		t.Fatalf("ParseDesignContextJSON: %v", err)
	}
	if len(designCtx.Files) != 1 {
		t.Fatalf("design context files = %d, want 1", len(designCtx.Files))
	}
	if designCtx.Files[0].Source != contextPath || designCtx.Files[0].Content != "ship the agreed contract" {
		t.Fatalf("design context file = %+v", designCtx.Files[0])
	}
}

func TestPushReceivedFailsTheRunOnAMissingGlobalDesignContextFile(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})
	missing := filepath.Join(t.TempDir(), "absent.md")
	globalConfig, err := os.ReadFile(p.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	globalConfig = append(globalConfig, []byte(fmt.Sprintf("\ndesign_context:\n  files:\n    - %q\n", missing))...)
	if err := os.WriteFile(p.ConfigFile(), globalConfig, 0o644); err != nil {
		t.Fatal(err)
	}

	_, headSHA := setupTestGitRepo(t, p, d, "design-context-missing")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("design-context-missing"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err == nil {
		t.Fatal("run started without its configured global design context")
	}
	if step.execCnt.Load() != 0 {
		t.Fatal("a step ran without its configured global design context")
	}
}
