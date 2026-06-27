//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPerPushRouteSelectsParentTarget proves the per-push route feature:
// a gate initialized WITHOUT a fork (so its implicit default target is the
// parent only) gains two local routes, and a push selecting
// `-o no-mistakes.route=parent` resolves base=parent / head=fork — opening the
// PR against the parent while pushing the branch to the fork, all from one
// clone with no re-init.
//
// The base=parent / head=fork resolution is asserted deterministically from the
// daemon's "route resolved" log plus the real fork push (the branch lands on
// the fork, never the parent). The parent-PR assertion is additionally checked
// when the (stubbed) gh CLI is authenticated; on platforms where the daemon's
// resolved login-shell environment drops the e2e gh-stub mode (a pre-existing
// macOS limitation shared with TestForkRouting), the PR step skips and that
// sub-assertion is skipped too.
func TestPerPushRouteSelectsParentTarget(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	ctx := context.Background()

	parentURL := "https://github.com/parent-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	branch := "feature/per-push-route-e2e"

	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}

	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set parent origin: %v\n%s", err, out)
	}

	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-per-push-route.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "parent-owner/no-mistakes")

	// Initialize WITHOUT --fork-url: the implicit default target is the parent
	// only (no fork). Routes are layered on locally afterward.
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	// Two local routes: "parent" redirects PRs to the parent while pushing the
	// branch to the fork; "self" targets the fork directly. The push selects
	// "parent".
	if out, err := h.Run("route", "add", "parent", "--base", parentURL, "--fork-url", forkURL); err != nil {
		t.Fatalf("route add parent: %v\n%s", err, out)
	}
	if out, err := h.Run("route", "add", "self", "--base", forkURL); err != nil {
		t.Fatalf("route add self: %v\n%s", err, out)
	}
	if out, err := h.Run("route", "list"); err != nil {
		t.Fatalf("route list: %v\n%s", err, out)
	} else if !strings.Contains(out, "parent") || !strings.Contains(out, "self") {
		t.Fatalf("route list missing routes: %s", out)
	}

	h.CommitChange(branch, "route.txt", "per-push route\n", "add per-push route")
	h.PushToGateWithOptions(branch, "no-mistakes.route=parent")

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	// head=fork: the selected route's fork received the branch at the run head.
	forkSHA, err := h.runGit(ctx, forkDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("fork branch missing (route did not push to fork): %v\n%s", err, forkSHA)
	}
	if got := string(bytes.TrimSpace(forkSHA)); got != run.HeadSHA {
		t.Fatalf("fork branch SHA = %s, want run head %s", got, run.HeadSHA)
	}
	// The parent must NOT have received the feature branch.
	if out, err := h.runGit(ctx, h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("parent unexpectedly received feature branch at %s", bytes.TrimSpace(out))
	}

	// base=parent / head=fork: the daemon resolved the selected route to the
	// parent base and fork head before the run started. This is the direct,
	// platform-independent proof of resolution.
	assertRouteResolved(t, h, "parent", parentURL, forkURL)

	// When the stubbed gh CLI is authenticated in this environment, the PR step
	// runs: assert it created a PR against the parent with the fork-owner head.
	if _, statErr := os.Stat(ghLog); statErr != nil {
		t.Logf("gh stub not exercised (PR step skipped in this environment); resolution proven via daemon log + fork push")
		return
	}
	invocations := readGHStubInvocations(t, ghLog)
	var sawParentCreate bool
	for _, inv := range invocations {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			if inv.Repo == "fork-owner/no-mistakes" {
				t.Fatalf("created silent self-PR against fork: %+v", inv)
			}
			if inv.Repo == "parent-owner/no-mistakes" && inv.Head == "fork-owner:"+branch && inv.Base == "main" {
				sawParentCreate = true
			}
		}
	}
	if !sawParentCreate {
		t.Fatalf("did not see parent PR create with fork owner head in gh log: %+v", invocations)
	}
}

// assertRouteResolved checks the daemon log recorded the selected route
// resolving to the expected base and fork URLs.
func assertRouteResolved(t *testing.T, h *Harness, routeName, baseURL, forkURL string) {
	t.Helper()
	var data []byte
	for _, cand := range []string{
		filepath.Join(h.NMHome, "logs", "daemon.log"),
		filepath.Join(h.NMHome, "daemon.log"),
	} {
		if b, err := os.ReadFile(cand); err == nil && len(b) > 0 {
			data = b
			break
		}
	}
	if len(data) == 0 {
		t.Fatal("daemon log not found; cannot assert route resolution")
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "route resolved") {
			continue
		}
		if strings.Contains(line, "route="+routeName) &&
			strings.Contains(line, "base="+baseURL) &&
			strings.Contains(line, "fork="+forkURL) {
			return
		}
	}
	t.Fatalf("daemon log has no 'route resolved' entry for route=%s base=%s fork=%s\n%s", routeName, baseURL, forkURL, data)
}
