package steps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// A large push is one long upload, and over a flaky path each attempt fails
// again from the start (measured: 12 MB packs failed ~45% of the time to
// GitHub, small ones did not). When the objects the remote lacks exceed
// largePushThreshold, the branch's new commits are first pushed oldest-first in
// pieces to a scratch ref outside refs/heads, each piece with the usual
// transport retry. The ordinary branch push then sends only what is left.
//
// This is a var so tests can shrink it.
var largePushThreshold int64 = 5 << 20

// scratchRef is the throwaway ref the pieces go to. It is not under
// refs/heads, so it fires no `on: push` workflow or branch rule in the target.
func scratchRef(runID string) string { return "refs/no-mistakes/tmp/" + runID }

// pushLargeBranchInPieces sends head's new commits to the scratch ref when
// their pack would exceed largePushThreshold. The returned cleanup deletes the
// scratch ref (best effort, logged) and is never nil; call it after the branch
// push, whatever its result.
//
// Sizing is an optimisation: when it cannot be measured the piecewise push is
// skipped and the ordinary push runs as before. Only a failed piece push is an
// error.
func pushLargeBranchInPieces(sctx *pipeline.StepContext, pushURL, head string) (cleanup func(), err error) {
	cleanup = func() {}
	tips, err := presentRemoteTips(sctx, pushURL)
	if err != nil {
		sctx.Log(fmt.Sprintf("push: not splitting the push, could not read remote branches: %v", err))
		return cleanup, nil
	}
	total, err := newObjectBytes(sctx, sctx.Ctx, head, tips)
	if err != nil {
		sctx.Log(fmt.Sprintf("push: not splitting the push, could not measure it: %v", err))
		return cleanup, nil
	}
	if total <= largePushThreshold {
		return cleanup, nil
	}
	pieces, err := planPushPieces(sctx, head, tips)
	if err != nil {
		sctx.Log(fmt.Sprintf("push: not splitting the push, could not plan the pieces: %v", err))
		return cleanup, nil
	}
	if len(pieces) < 2 {
		return cleanup, nil
	}

	ref := scratchRef(sctx.Run.ID)
	sctx.Log(fmt.Sprintf("push: %d MB of new objects, sending %d pieces through %s first", total>>20, len(pieces)-1, ref))
	cleanup = func() { deleteScratchRef(sctx, pushURL, ref) }
	// The last piece ends at head; the branch push sends it.
	for i, piece := range pieces[:len(pieces)-1] {
		tip := piece[len(piece)-1]
		sctx.Log(fmt.Sprintf("push: piece %d/%d (%d commits, up to %s)", i+1, len(pieces)-1, len(piece), shortSHA(tip)))
		if err := pushPiece(sctx, pushURL, tip, ref); err != nil {
			return cleanup, fmt.Errorf("push piece %d/%d to %s: %w", i+1, len(pieces)-1, ref, err)
		}
	}
	return cleanup, nil
}

// pushPiece force-pushes tip to the scratch ref. Pieces of a merge history are
// not always descendants of one another, and the ref is this run's own, so the
// leading "+" costs nothing.
func pushPiece(sctx *pipeline.StepContext, pushURL, tip, ref string) error {
	return retryTransport(sctx, "push piece", func(ctx context.Context) error {
		return stepGitPushCommit(sctx, ctx, pushURL, "+"+tip, ref, "", false)
	}, func(ctx context.Context) (bool, error) {
		remote, err := lsRemoteSHA(ctxGitRunner(sctx, ctx), pushURL, ref)
		if err != nil {
			return false, err
		}
		return remote == tip, nil
	})
}

// deleteScratchRef removes the scratch ref if it exists. It outlives a
// cancelled run's context so a failed or cancelled push still cleans up; a
// failure is only logged, since a leftover ref is harmless.
func deleteScratchRef(sctx *pipeline.StepContext, pushURL, ref string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(sctx.Ctx), time.Minute)
	defer cancel()
	remote, err := lsRemoteSHA(ctxGitRunner(sctx, ctx), pushURL, ref)
	if err == nil && remote == "" {
		return
	}
	if _, err := stepGitRunRawCtx(sctx, ctx, "push", pushURL, "--delete", ref); err != nil {
		sctx.Log(fmt.Sprintf("push: could not delete scratch ref %s, delete it by hand: %v", ref, err))
		return
	}
	sctx.Log(fmt.Sprintf("push: deleted scratch ref %s", ref))
}

// presentRemoteTips returns the remote's branch tips this clone already has;
// objects reachable from them are on the remote and are not sent again.
func presentRemoteTips(sctx *pipeline.StepContext, pushURL string) ([]string, error) {
	ctx, cancel := context.WithTimeout(sctx.Ctx, transportAttemptTimeout)
	defer cancel()
	out, err := stepGitRunRawCtx(sctx, ctx, "ls-remote", "--heads", pushURL)
	if err != nil {
		return nil, err
	}
	var shas []string
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			shas = append(shas, fields[0])
		}
	}
	if len(shas) == 0 {
		return nil, nil
	}
	var checked bytes.Buffer
	if err := stepGitStream(sctx, ctx, strings.Join(shas, "\n")+"\n", &checked, "cat-file", "--batch-check=%(objectname) %(objecttype)"); err != nil {
		return nil, err
	}
	var present []string
	for _, line := range strings.Split(checked.String(), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == "commit" {
			present = append(present, fields[0])
		}
	}
	return present, nil
}

// newObjectBytes is the size of the pack a push of head would send when the
// remote already has everything reachable from exclude.
func newObjectBytes(sctx *pipeline.StepContext, ctx context.Context, head string, exclude []string) (int64, error) {
	var in strings.Builder
	in.WriteString(head + "\n")
	for _, e := range exclude {
		in.WriteString("^" + e + "\n")
	}
	var n countingWriter
	if err := stepGitStream(sctx, ctx, in.String(), &n, "pack-objects", "--revs", "--stdout", "--thin", "-q"); err != nil {
		return 0, err
	}
	return int64(n), nil
}

type countingWriter int64

func (c *countingWriter) Write(p []byte) (int, error) {
	*c += countingWriter(len(p))
	return len(p), nil
}

// planPushPieces splits head's new commits, oldest first, into pieces whose
// packs stay under the threshold.
func planPushPieces(sctx *pipeline.StepContext, head string, remoteTips []string) ([][]string, error) {
	args := []string{"rev-list", "--reverse", "--topo-order", "--parents", head}
	if len(remoteTips) > 0 {
		args = append(args, "--not")
		args = append(args, remoteTips...)
	}
	out, err := stepGitRun(sctx, args...)
	if err != nil {
		return nil, err
	}
	commits, isChain := newCommitChain(out)
	if !isChain {
		sctx.Log("push: not splitting the push, the new commits are not a single chain")
		return nil, nil
	}
	return piecePlan(commits, largePushThreshold, func(prev, tip string) (int64, error) {
		exclude := remoteTips
		if prev != "" {
			exclude = append(append([]string(nil), remoteTips...), prev)
		}
		return newObjectBytes(sctx, sctx.Ctx, tip, exclude)
	})
}

// newCommitChain parses `rev-list --reverse --parents` output and reports
// whether the commits form a single chain: each one's parents among the new
// commits are exactly the one before it. Parents the remote already has are
// ignored. Only a chain can be staged through one scratch ref, since each
// piece's tip then contains every earlier piece.
func newCommitChain(revListParents string) ([]string, bool) {
	var commits []string
	parents := map[string][]string{}
	for _, line := range strings.Split(revListParents, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		commits = append(commits, fields[0])
		parents[fields[0]] = fields[1:]
	}
	isNew := make(map[string]bool, len(commits))
	for _, c := range commits {
		isNew[c] = true
	}
	for i, c := range commits {
		var inNew []string
		for _, p := range parents[c] {
			if isNew[p] {
				inNew = append(inNew, p)
			}
		}
		if i == 0 && len(inNew) != 0 || i > 0 && (len(inNew) != 1 || inNew[0] != commits[i-1]) {
			return nil, false
		}
	}
	return commits, true
}

// piecePlan groups commits (oldest to newest) greedily: a piece grows while the
// pack for it, measured by size(previous piece's tip, candidate tip), stays at
// or under threshold. A single commit over the threshold is a piece of its own;
// it cannot be split further.
func piecePlan(commits []string, threshold int64, size func(prev, tip string) (int64, error)) ([][]string, error) {
	var pieces [][]string
	var cur []string
	prev := ""
	for _, c := range commits {
		if len(cur) > 0 {
			n, err := size(prev, c)
			if err != nil {
				return nil, err
			}
			if n > threshold {
				pieces = append(pieces, cur)
				prev = cur[len(cur)-1]
				cur = nil
			}
		}
		cur = append(cur, c)
	}
	if len(cur) > 0 {
		pieces = append(pieces, cur)
	}
	return pieces, nil
}

// stepGitStream runs git with stdin and streams stdout to w, under the step's
// environment and a process-tree-killing runner (see stepGitRunRawCtx).
func stepGitStream(sctx *pipeline.StepContext, ctx context.Context, stdin string, w io.Writer, args ...string) error {
	cmd := stepCmdContext(sctx, ctx, "git", args...)
	cmd.Env = git.NonInteractiveEnvFrom(cmd.Env, sctx.WorkDir)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	shellenv.ConfigureShellCommand(cmd)
	if err := shellenv.RunShellCommand(cmd); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, safeurl.RedactText(strings.TrimSpace(stderr.String())))
	}
	return nil
}
