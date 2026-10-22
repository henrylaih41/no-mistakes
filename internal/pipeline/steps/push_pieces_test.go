package steps

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// smallLargePushThreshold makes 2 MB commits "large" without sending 5 MB
// packs in tests.
func smallLargePushThreshold(t *testing.T, bytes int64) {
	t.Helper()
	old := largePushThreshold
	largePushThreshold = bytes
	t.Cleanup(func() { largePushThreshold = old })
}

// pushRefspecs returns the refspec of every `git push` in the fake git log.
func pushRefspecs(t *testing.T, logFile string) []string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	var specs []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "push ") {
			continue
		}
		fields := strings.Fields(line)
		specs = append(specs, fields[len(fields)-1])
	}
	return specs
}

func TestPiecePlan(t *testing.T) {
	// Each commit adds 2 units on top of the previous piece's tip.
	size := func(index map[string]int) func(prev, tip string) (int64, error) {
		return func(prev, tip string) (int64, error) {
			from := -1
			if prev != "" {
				from = index[prev]
			}
			return int64(2 * (index[tip] - from)), nil
		}
	}
	commits := []string{"a", "b", "c", "d", "e"}
	index := map[string]int{"a": 0, "b": 1, "c": 2, "d": 3, "e": 4}
	cases := []struct {
		name      string
		threshold int64
		want      string
	}{
		{"everything fits in one piece", 100, "a b c d e"},
		{"two commits per piece", 4, "a b|c d|e"},
		{"each commit over the threshold is its own piece", 1, "a|b|c|d|e"},
	}
	for _, tc := range cases {
		pieces, err := piecePlan(commits, tc.threshold, size(index))
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, p := range pieces {
			parts = append(parts, strings.Join(p, " "))
		}
		if got := strings.Join(parts, "|"); got != tc.want {
			t.Errorf("%s: pieces = %q, want %q", tc.name, got, tc.want)
		}
	}
	if pieces, err := piecePlan(nil, 4, size(index)); err != nil || len(pieces) != 0 {
		t.Errorf("no commits: pieces=%v err=%v, want none", pieces, err)
	}
	boom := errors.New("pack-objects failed")
	if _, err := piecePlan(commits, 4, func(string, string) (int64, error) { return 0, boom }); !errors.Is(err, boom) {
		t.Errorf("a sizing error must surface, got %v", err)
	}
}

func TestPushStep_LargeBranchArrivesInPiecesThroughTheScratchRef(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	smallLargePushThreshold(t, 3<<20)
	const mb = 1 << 20
	sctx, upstream, head, logFile := pushRetryFixture(t, nil, 2*mb, 2*mb, 2*mb)
	logs := captureLog(sctx)
	scratch := scratchRef(sctx.Run.ID)
	workDir := sctx.WorkDir
	commits := strings.Fields(gitCmd(t, workDir, "rev-list", "--reverse", "feature~3..feature"))
	if len(commits) != 3 || commits[2] != head {
		t.Fatalf("fixture commits = %v, head %s", commits, head)
	}

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"+" + commits[0] + ":" + scratch,
		"+" + commits[1] + ":" + scratch,
		head + ":refs/heads/feature",
		scratch, // push --delete
	}
	got := pushRefspecs(t, logFile)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("push refspecs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != head {
		t.Errorf("remote feature = %s, want %s", remote, head)
	}
	if refs := gitCmd(t, upstream, "for-each-ref", "refs/no-mistakes"); refs != "" {
		t.Errorf("scratch ref left on the remote:\n%s", refs)
	}
	if !strings.Contains(logs(), "deleted scratch ref "+scratch) {
		t.Errorf("step log should record the scratch ref deletion:\n%s", logs())
	}
}

func TestPushStep_SmallBranchIsOneOrdinaryPush(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	smallLargePushThreshold(t, 3<<20)
	sctx, upstream, head, logFile := pushRetryFixture(t, nil)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got := pushRefspecs(t, logFile); len(got) != 1 || got[0] != head+":refs/heads/feature" {
		t.Errorf("push refspecs = %v, want one ordinary branch push", got)
	}
	if refs := gitCmd(t, upstream, "for-each-ref", "refs/no-mistakes"); refs != "" {
		t.Errorf("a small push must not touch the scratch namespace:\n%s", refs)
	}
}

func TestPushStep_ScratchRefIsDeletedWhenTheBranchPushFails(t *testing.T) {
	fastTransportRetry(t, time.Minute)
	smallLargePushThreshold(t, 3<<20)
	const mb = 1 << 20
	sctx, upstream, _, logFile := pushRetryFixture(t, map[string]string{"FAKE_CLI_PUSH_REJECT_PREFIX": "refs/heads/"}, 2*mb, 2*mb, 2*mb)
	scratch := scratchRef(sctx.Run.ID)

	if _, err := (&PushStep{}).Execute(sctx); err == nil {
		t.Fatal("a rejected branch push must fail the step")
	}
	var scratchPushes int
	for _, spec := range pushRefspecs(t, logFile) {
		if strings.HasSuffix(spec, ":"+scratch) {
			scratchPushes++
		}
	}
	if scratchPushes != 2 {
		t.Errorf("scratch pushes = %d, want 2 pieces before the failing branch push", scratchPushes)
	}
	if refs := gitCmd(t, upstream, "for-each-ref", "refs/no-mistakes"); refs != "" {
		t.Errorf("scratch ref left on the remote after a failed run:\n%s", refs)
	}
}
