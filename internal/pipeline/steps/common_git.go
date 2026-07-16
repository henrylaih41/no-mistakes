package steps

import (
	"context"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// reviewWorkload returns the bounded change size (files + net lines) between
// base and head for local telemetry, or nil when the diff-stat cannot be
// computed (so the invocation records an unknown workload rather than a
// fabricated zero).
func reviewWorkload(ctx context.Context, workDir, base, head string) *agent.InvocationWorkload {
	files, lines, err := git.DiffStat(ctx, workDir, base, head)
	if err != nil {
		return nil
	}
	return &agent.InvocationWorkload{Files: files, Lines: lines}
}

// resolveBaseSHA returns a usable base SHA for diff/log operations.
// When baseSHA is the zero ref (new branch push), it tries git merge-base
// against the default branch, falling back to the empty tree SHA.
func resolveBaseSHA(ctx context.Context, workDir, baseSHA, defaultBranch string) string {
	if !git.IsZeroSHA(baseSHA) {
		return baseSHA
	}
	if mb := mergeBaseWithDefaultBranch(ctx, workDir, defaultBranch); mb != "" {
		return mb
	}
	return git.EmptyTreeSHA
}

// resolveBranchBaseSHA returns the branch base commit relative to the default
// branch when possible. This keeps pipeline steps scoped to the full branch,
// not just the last pushed delta. If merge-base cannot be determined, it falls
// back to resolveBaseSHA.
func resolveBranchBaseSHA(ctx context.Context, workDir, fallbackBaseSHA, defaultBranch string) string {
	if mb := mergeBaseWithDefaultBranch(ctx, workDir, defaultBranch); mb != "" {
		return mb
	}
	return resolveBaseSHA(ctx, workDir, fallbackBaseSHA, defaultBranch)
}

func resolveDefaultBranchTipSHA(ctx context.Context, workDir, upstreamURL, fallbackBaseSHA, defaultBranch string) string {
	sha, _ := resolveDefaultBranchTip(ctx, workDir, upstreamURL, fallbackBaseSHA, defaultBranch)
	return sha
}

func resolveDefaultBranchTip(ctx context.Context, workDir, upstreamURL, fallbackBaseSHA, defaultBranch string) (string, bool) {
	if strings.TrimSpace(defaultBranch) != "" {
		baseRemote := remoteOrURL(upstreamURL)
		localRef := baseTrackingRef(defaultBranch)
		if err := git.FetchRemoteBranchToRef(ctx, workDir, baseRemote, defaultBranch, localRef); err != nil {
			return unresolvedDefaultBranchTip(ctx, workDir, fallbackBaseSHA, defaultBranch), false
		}
		for _, ref := range defaultBranchRefCandidates(defaultBranch) {
			sha, err := git.Run(ctx, workDir, "rev-parse", "--verify", ref)
			if err == nil && strings.TrimSpace(sha) != "" {
				return strings.TrimSpace(sha), true
			}
		}
	}
	return resolveBaseSHA(ctx, workDir, fallbackBaseSHA, defaultBranch), false
}

func unresolvedDefaultBranchTip(ctx context.Context, workDir, fallbackBaseSHA, defaultBranch string) string {
	if !git.IsZeroSHA(fallbackBaseSHA) {
		return fallbackBaseSHA
	}
	sha, localErr := git.Run(ctx, workDir, "rev-parse", "--verify", defaultBranch)
	if localErr == nil && strings.TrimSpace(sha) != "" {
		return strings.TrimSpace(sha)
	}
	return git.EmptyTreeSHA
}

func mergeBaseWithDefaultBranch(ctx context.Context, workDir, defaultBranch string) string {
	if strings.TrimSpace(defaultBranch) == "" {
		return ""
	}
	for _, ref := range defaultBranchRefCandidates(defaultBranch) {
		mb, err := git.Run(ctx, workDir, "merge-base", "HEAD", ref)
		if err == nil && strings.TrimSpace(mb) != "" {
			return strings.TrimSpace(mb)
		}
	}
	return ""
}

// lastFetchedBranchTip returns the commit the push branch's remote-tracking ref
// resolves to in the worktree - the exact remote head the rebase step last
// fetched and rebased against. It is the safe anchor for a force-with-lease: if
// the live remote has moved past it, the push must be treated as potentially
// discarding unseen work. Returns "" when no tracking ref exists (e.g. a brand
// new branch or a failed fetch), which makes the caller fall back to the
// content-incorporation check rather than trusting a stale value.
func lastFetchedBranchTip(ctx context.Context, workDir, branch string, fork bool) string {
	trackingRef := "refs/remotes/origin/" + branch
	if fork {
		trackingRef = forkBranchTrackingRef(branch)
	}
	sha, err := git.Run(ctx, workDir, "rev-parse", "--verify", "--quiet", trackingRef+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(sha)
}

// baseTrackingRefPrefix namespaces the local refs a run fetches its base
// repository's branches into. refs/worktree/* refs are PER-WORKTREE: git keeps
// them in the worktree's private ref store, invisible to other linked worktrees
// that share the same bare repo. Runs are only serialized per repo+branch and a
// route can resolve a base URL that differs from the gate origin, so fetching
// base branches into the SHARED refs/remotes/origin/* namespace let concurrent
// runs in sibling worktrees clobber one another's base view between fetch and
// rebase/CI. Per-worktree refs keep each run's base isolated.
const baseTrackingRefPrefix = "refs/worktree/no-mistakes-base/"

func baseTrackingRef(branch string) string {
	return baseTrackingRefPrefix + branch
}

// defaultBranchRefCandidates lists the refs to consult for the base default
// branch, most-authoritative first: the per-worktree base ref the run fetched,
// then the shared remote-tracking ref, then a local branch of the same name.
func defaultBranchRefCandidates(defaultBranch string) []string {
	return []string{baseTrackingRef(defaultBranch), "origin/" + defaultBranch, defaultBranch}
}

func normalizedBranchRef(ref string) string {
	if !strings.HasPrefix(ref, "refs/") {
		return "refs/heads/" + ref
	}
	return ref
}
