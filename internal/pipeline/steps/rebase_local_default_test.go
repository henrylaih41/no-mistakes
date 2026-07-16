package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #283: a gated branch built on top of a local default branch that is
// ahead of origin carries that other workstream's committed-but-unpushed
// commits. Rebasing onto the fresh remote default branch keeps them in the
// branch's history, so the PR silently bundles unrelated work.
//
// The rebase step must detect that the branch carries commits which exist on
// the local default branch but were never pushed to origin/<default>, and stop
// for review instead of silently widening the PR.
func TestRebaseStep_DetectsUnpushedLocalDefaultBranchCommits(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	// Working repo: local default branch (main) advances with an unrelated
	// workstream's commit that is never pushed to origin.
	working := t.TempDir()
	gitCmd(t, working, "init")
	gitCmd(t, working, "config", "user.name", "test")
	gitCmd(t, working, "config", "user.email", "test@test.com")
	gitCmd(t, working, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(working, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "base")
	d0 := gitCmd(t, working, "rev-parse", "HEAD")
	gitCmd(t, working, "remote", "add", "origin", upstream)
	gitCmd(t, working, "push", "origin", "main") // origin/main == D0

	// Unrelated workstream commits to local main but does NOT push.
	os.WriteFile(filepath.Join(working, "unrelated_a.txt"), []byte("backend a"), 0o644)
	os.WriteFile(filepath.Join(working, "unrelated_b.txt"), []byte("backend b"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "unrelated backend work (77 files)")
	localMainTip := gitCmd(t, working, "rev-parse", "HEAD") // D0 + U, unpushed

	// Gate worktree: feature was branched off the local (ahead) main, so it
	// carries the unrelated commit U as an ancestor.
	dir := t.TempDir()
	gitCmd(t, dir, "clone", upstream, ".")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "fetch", working, "main") // import U's objects (as feature ancestor)
	gitCmd(t, dir, "checkout", "--detach", localMainTip)
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "my_fix.txt"), []byte("my 2-line fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "my fix")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD") // D0 + U + M

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, d0, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Repo.WorkingPath = working

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected the rebase step to stop for review when the branch bundles unpushed local-default commits, got outcome=%#v", outcome)
	}
	if outcome.AutoFixable {
		t.Fatalf("bundling unpushed local-default commits is not safely auto-fixable")
	}
	if !strings.Contains(outcome.Findings, "unrelated backend work") &&
		!strings.Contains(strings.ToLower(outcome.Findings), "local") {
		t.Fatalf("expected findings to flag the unpushed local-default-branch commits, got: %s", outcome.Findings)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected one finding, got %d", len(findings.Items))
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want %q", findings.Items[0].Action, types.ActionAskUser)
	}
}

// A selected route can point the base at a repo other than the gate origin.
// The bundled-local-default check must compare against the base the run rebases
// onto (baseTrackingRef), not the gate's origin/<default>: otherwise commits
// that are already on origin but absent from the routed base are silently
// bundled into the routed PR. Here origin carries the local-default commit but
// the routed base does NOT, so the check must still flag it.
func TestDetectBundledLocalDefaultCommits_RouteBaseBehindOrigin(t *testing.T) {
	t.Parallel()
	routeBase := t.TempDir()
	gitCmd(t, routeBase, "init", "--bare")

	// Working repo: local main starts at R0 (pushed to the routed base), then an
	// unrelated workstream commits U to local main without pushing it to the base.
	working := t.TempDir()
	gitCmd(t, working, "init")
	gitCmd(t, working, "config", "user.name", "test")
	gitCmd(t, working, "config", "user.email", "test@test.com")
	gitCmd(t, working, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(working, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "base")
	gitCmd(t, working, "remote", "add", "base", routeBase)
	gitCmd(t, working, "push", "base", "main") // routeBase main == R0

	os.WriteFile(filepath.Join(working, "unrelated.txt"), []byte("unrelated work"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "unrelated workstream commit")
	localMainTip := gitCmd(t, working, "rev-parse", "HEAD") // R0 + U, never on routeBase

	// Gate worktree: feature branched off local main (carries U as ancestor).
	dir := t.TempDir()
	gitCmd(t, dir, "clone", routeBase, ".")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "fetch", working, "main")
	gitCmd(t, dir, "checkout", "--detach", localMainTip)
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "my_fix.txt"), []byte("fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "my fix")

	// The gate's own origin/main DOES carry U: the old origin/<default> check would
	// not flag it. The routed base (baseTrackingRef) is still at R0, so the route-
	// aware check must flag U.
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/main", localMainTip)
	gitCmd(t, dir, "update-ref", baseTrackingRef("main"), gitCmd(t, dir, "rev-parse", "refs/remotes/origin/main~1"))

	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, "", "", config.Commands{})
	sctx.Repo.WorkingPath = working
	sctx.Repo.DefaultBranch = "main"

	outcome := detectBundledLocalDefaultCommits(sctx.Ctx, sctx, "feature", "main")
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected the routed-base check to flag commits absent from the routed base, got %#v", outcome)
	}
	if !strings.Contains(outcome.Findings, "unrelated workstream commit") {
		t.Fatalf("expected findings to name the bundled commit, got: %s", outcome.Findings)
	}
}

func TestRebaseStep_DetectsUnpushedLocalDefaultBranchCommitsOnForcePush(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	working := t.TempDir()
	gitCmd(t, working, "init")
	gitCmd(t, working, "config", "user.name", "test")
	gitCmd(t, working, "config", "user.email", "test@test.com")
	gitCmd(t, working, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(working, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "base")
	gitCmd(t, working, "remote", "add", "origin", upstream)
	gitCmd(t, working, "push", "origin", "main")

	os.WriteFile(filepath.Join(working, "unrelated_force.txt"), []byte("local main work"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "unrelated local main work")
	localMainTip := gitCmd(t, working, "rev-parse", "HEAD")

	dir := t.TempDir()
	gitCmd(t, dir, "clone", upstream, ".")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "old_feature.txt"), []byte("old feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "old feature")
	oldFeatureSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	gitCmd(t, dir, "fetch", working, "main")
	gitCmd(t, dir, "checkout", "--detach", localMainTip)
	gitCmd(t, dir, "checkout", "-B", "feature")
	os.WriteFile(filepath.Join(dir, "my_force_fix.txt"), []byte("force-pushed fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "force-pushed fix")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, oldFeatureSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Repo.WorkingPath = working

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected force-push rebase to stop for bundled local default commits, got outcome=%#v", outcome)
	}
	if outcome.AutoFixable {
		t.Fatalf("bundled local default commits on a force push are not safely auto-fixable")
	}
	if !strings.Contains(outcome.Findings, "unrelated local main work") {
		t.Fatalf("expected findings to mention the bundled local main commit, got: %s", outcome.Findings)
	}
}
