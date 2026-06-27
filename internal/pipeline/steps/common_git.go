package steps

import (
	"context"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

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
