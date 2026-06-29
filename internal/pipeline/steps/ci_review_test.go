package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// fakeReviewHost is a minimal scm.Host that serves canned review verdict/findings
// (and records the args the loop calls them with). Only the review methods carry
// behavior; the rest satisfy the interface and are unused by the review loop.
type fakeReviewHost struct {
	caps     scm.Capabilities
	verdict  scm.ReviewVerdict
	verdErr  error
	findings []scm.ReviewComment
	findErr  error
	replyErr error // when set, every ReplyToReviewComment returns it

	verdictCalls  int
	findingsCalls int
	gotHeadSHA    string
	gotBotLogin   string
	gotPRNumber   int

	// replies records each ReplyToReviewComment call so tests can assert which
	// findings were acknowledged after a fix push.
	replies []reviewReply
}

// reviewReply captures one ReplyToReviewComment call.
type reviewReply struct {
	prNumber  int
	commentID int64
	body      string
}

func (h *fakeReviewHost) Provider() scm.Provider          { return scm.ProviderGitHub }
func (h *fakeReviewHost) Capabilities() scm.Capabilities  { return h.caps }
func (h *fakeReviewHost) Available(context.Context) error { return nil }
func (h *fakeReviewHost) FindPR(context.Context, string, string) (*scm.PR, error) {
	return nil, nil
}
func (h *fakeReviewHost) CreatePR(context.Context, string, string, scm.PRContent) (*scm.PR, error) {
	return nil, nil
}
func (h *fakeReviewHost) UpdatePR(context.Context, *scm.PR, scm.PRContent) (*scm.PR, error) {
	return nil, nil
}
func (h *fakeReviewHost) GetPRState(context.Context, *scm.PR) (scm.PRState, error) {
	return scm.PRStateOpen, nil
}
func (h *fakeReviewHost) GetChecks(context.Context, *scm.PR) ([]scm.Check, error) {
	return nil, nil
}
func (h *fakeReviewHost) GetMergeableState(context.Context, *scm.PR) (scm.MergeableState, error) {
	return scm.MergeableUnknown, scm.ErrUnsupported
}
func (h *fakeReviewHost) FetchFailedCheckLogs(context.Context, *scm.PR, string, string, []string) (string, error) {
	return "", scm.ErrUnsupported
}
func (h *fakeReviewHost) GetReviewVerdict(_ context.Context, prNumber int, headSHA, botLogin string) (scm.ReviewVerdict, []scm.ReviewComment, error) {
	h.verdictCalls++
	h.gotPRNumber = prNumber
	h.gotHeadSHA = headSHA
	h.gotBotLogin = botLogin
	if h.verdErr != nil {
		return h.verdict, nil, h.verdErr
	}
	return h.verdict, h.findings, nil
}
func (h *fakeReviewHost) GetBotFindings(context.Context, int, string, string) ([]scm.ReviewComment, error) {
	h.findingsCalls++
	return h.findings, h.findErr
}
func (h *fakeReviewHost) ReplyToReviewComment(_ context.Context, prNumber int, commentID int64, body string) error {
	h.replies = append(h.replies, reviewReply{prNumber: prNumber, commentID: commentID, body: body})
	return h.replyErr
}

func severeFinding() scm.ReviewComment {
	return scm.ReviewComment{Path: "a.go", Line: 10, Severity: "high", Body: "off-by-one"}
}

func TestEvalDevinGate(t *testing.T) {
	t.Parallel()
	enabled := config.ReviewLoop{Enabled: true, BotLogin: "bot", MaxRounds: 3, FailOpen: true}
	failClosed := config.ReviewLoop{Enabled: true, BotLogin: "bot", MaxRounds: 3, FailOpen: false}
	disabled := config.ReviewLoop{Enabled: false, FailOpen: true}

	tests := []struct {
		name     string
		verdict  scm.ReviewVerdict
		findings []scm.ReviewComment
		cfg      config.ReviewLoop
		elapsed  time.Duration
		want     devinGateDecision
	}{
		{"disabled is inert", scm.VerdictChangesRequested, []scm.ReviewComment{severeFinding()}, disabled, time.Hour, devinDecisionDisabled},
		{"approved is green", scm.VerdictApproved, nil, enabled, time.Hour, devinDecisionGreen},
		{"changes requested is not green", scm.VerdictChangesRequested, nil, enabled, time.Minute, devinDecisionNotGreen},
		// PR #5 regression: a PENDING verdict means Devin has NOT reviewed the
		// current head. The findings returned alongside a PENDING verdict are from
		// GetBotFindings, which does NOT filter by head SHA — so they are STALE
		// threads left over from an older head (the one the loop just fixed). The
		// verdict is the authoritative signal: CHANGES_REQUESTED already encodes
		// "Devin reviewed THIS head and found severe issues" (GetReviewVerdict
		// rolls severe findings on the head up to CHANGES_REQUESTED). Treating
		// stale severe findings on an unreviewed head as NotGreen drove a redundant
		// fix round on every poll until MaxRounds, posting a new "Addressed in
		// <sha>" reply on the same thread for each push. The loop must WAIT for
		// Devin to re-review the new head instead of re-fixing stale findings.
		{"stale severe finding on unreviewed head waits for re-review", scm.VerdictPending, []scm.ReviewComment{severeFinding()}, enabled, time.Minute, devinDecisionPending},
		{"pending within grace waits", scm.VerdictPending, nil, enabled, devinGraceWindow - time.Minute, devinDecisionPending},
		{"none within grace waits", scm.VerdictNone, nil, enabled, time.Minute, devinDecisionPending},
		{"silent past grace fails open", scm.VerdictNone, nil, enabled, devinGraceWindow + time.Minute, devinDecisionFailOpen},
		{"silent past grace fail-closed keeps waiting", scm.VerdictNone, nil, failClosed, devinGraceWindow + time.Minute, devinDecisionPending},
		{"low finding within grace is not severe", scm.VerdictPending, []scm.ReviewComment{{Path: "a.go", Line: 1, Severity: "low", Body: "nit"}}, enabled, time.Minute, devinDecisionPending},
		{"top-level summary is not severe", scm.VerdictPending, []scm.ReviewComment{{Severity: "high", Body: "overall looks risky"}}, enabled, time.Minute, devinDecisionPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalDevinGate(tt.verdict, tt.findings, tt.cfg, tt.elapsed); got != tt.want {
				t.Errorf("evalDevinGate(%s) = %v, want %v", tt.verdict, got, tt.want)
			}
		})
	}
}

func TestHasUnresolvedSevereFindings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []scm.ReviewComment
		want bool
	}{
		{"nil", nil, false},
		{"file-scoped high", []scm.ReviewComment{severeFinding()}, true},
		{"file-scoped medium", []scm.ReviewComment{{Path: "x", Line: 2, Severity: "MEDIUM"}}, true},
		{"file-scoped low", []scm.ReviewComment{{Path: "x", Line: 2, Severity: "low"}}, false},
		{"top-level high ignored", []scm.ReviewComment{{Severity: "high", Body: "summary"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasUnresolvedSevereFindings(tc.in); got != tc.want {
				t.Errorf("hasUnresolvedSevereFindings = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDevinFindingFingerprints(t *testing.T) {
	t.Parallel()
	a := []scm.ReviewComment{
		{Path: "a.go", Line: 1, Severity: "high", Body: "x"},
		{Path: "b.go", Line: 2, Severity: "medium", Body: "y"},
	}
	// Order-independent and dedup-stable.
	reordered := []scm.ReviewComment{a[1], a[0], a[0]}
	if got, want := devinFindingFingerprints(a), devinFindingFingerprints(reordered); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fingerprints not order/dedup stable: %v vs %v", got, want)
	}
	// Rewording a finding body must NOT change its fingerprint: the fingerprint
	// keys on (Path, Line, Severity) only, so minor bot rewrites don't defeat
	// anti-thrash.
	reworded := []scm.ReviewComment{{Path: "a.go", Line: 1, Severity: "high", Body: "x2 reworded"}, a[1]}
	if strings.Join(devinFindingFingerprints(a), ",") != strings.Join(devinFindingFingerprints(reworded), ",") {
		t.Fatal("expected identical fingerprints when only the body changes")
	}
	// A changed severity (like a changed path or line) DOES produce a different
	// fingerprint set, so a genuinely different finding re-triggers a fix.
	changed := []scm.ReviewComment{{Path: "a.go", Line: 1, Severity: "medium", Body: "x"}, a[1]}
	if strings.Join(devinFindingFingerprints(a), ",") == strings.Join(devinFindingFingerprints(changed), ",") {
		t.Fatal("expected different fingerprints when a finding's severity changes")
	}
	if devinFindingFingerprints(nil) != nil {
		t.Fatal("expected nil fingerprints for no findings")
	}
}

func TestEncodeDevinFixKey(t *testing.T) {
	t.Parallel()
	// Empty inputs => empty key.
	if got := encodeDevinFixKey(nil, false, "", nil); got != "" {
		t.Fatalf("expected empty key, got %q", got)
	}
	// Devin-only key round-trips through decode (validity now considers prints).
	key := encodeDevinFixKey(nil, false, "deadbeef", []string{"fp1", "fp2"})
	if key == "" {
		t.Fatal("expected non-empty Devin key")
	}
	issues, ok := decodeLastFixedChecks(key)
	if !ok {
		t.Fatal("expected Devin-only key to decode as valid")
	}
	if issues.HeadSHA != "deadbeef" || len(issues.DevinPrints) != 2 {
		t.Fatalf("decoded key mismatch: %+v", issues)
	}
	// Different head SHA => different key (so a new commit re-evaluates).
	if encodeDevinFixKey(nil, false, "cafe", []string{"fp1", "fp2"}) == key {
		t.Fatal("expected different key for a different head SHA")
	}
}

func TestEncodeLastFixedChecksByteIdenticalWhenDevinAbsent(t *testing.T) {
	t.Parallel()
	// The check/merge-conflict key must marshal identically whether produced by
	// the legacy encoder or the Devin encoder with empty Devin inputs, so the
	// review-loop-disabled anti-thrash bytes are unchanged.
	legacy := encodeLastFixedChecks([]string{"build", "lint"}, true)
	withDevin := encodeDevinFixKey([]string{"build", "lint"}, true, "", nil)
	if legacy != withDevin {
		t.Fatalf("key drift: legacy=%q devin=%q", legacy, withDevin)
	}
}

func TestCIFixAlreadyAttempted_SeparatesCIAndReviewDimensions(t *testing.T) {
	t.Parallel()

	failing := []string{"build", "lint"}
	ciKey := encodeLastFixedChecks(failing, true)

	// Mixed CI+review fix just pushed against head1 with finding fp1.
	s := &CIStep{}
	s.recordCIFix(ciKey, encodeDevinFixKey(nil, false, "head1", []string{"fp1"}), true, nil)
	if s.lastFixedChecks != ciKey {
		t.Fatalf("recordCIFix stored CI key %q, want %q", s.lastFixedChecks, ciKey)
	}

	// Same CI failures + same review finding on the same head: already attempted.
	if !s.ciFixAlreadyAttempted(ciKey, encodeDevinFixKey(nil, false, "head1", []string{"fp1"}), true) {
		t.Fatal("same CI failures + same review finding must read as already attempted")
	}

	// The regression: the freshly pushed head usually makes the review verdict go
	// pending (devinNotGreen=false) while GitHub still reports the SAME stale
	// failing checks. The CI anti-thrash must stay stable across that transition
	// so a second fix does not run before CI re-runs.
	if !s.ciFixAlreadyAttempted(ciKey, "", false) {
		t.Fatal("CI anti-thrash must survive the review not-green -> pending transition")
	}

	// A genuinely new review finding on the same failing checks must re-trigger.
	if s.ciFixAlreadyAttempted(ciKey, encodeDevinFixKey(nil, false, "head1", []string{"fp1", "fp2"}), true) {
		t.Fatal("a new review fingerprint set must not read as already attempted")
	}

	// A different failing-check set must re-trigger regardless of review state.
	otherCI := encodeLastFixedChecks([]string{"deploy"}, false)
	if s.ciFixAlreadyAttempted(otherCI, "", false) {
		t.Fatal("a different CI-check identity must not read as already attempted")
	}
}

func TestDevinFindingsPromptSection(t *testing.T) {
	t.Parallel()
	if got := devinFindingsPromptSection(nil); got != "" {
		t.Fatalf("expected empty section for no findings, got %q", got)
	}
	got := devinFindingsPromptSection([]scm.ReviewComment{
		{Path: "a.go", Line: 12, Severity: "high", Body: "leak"},
		{Severity: "", Body: "general note"},
		{Body: "   "}, // blank body skipped
	})
	if !strings.Contains(got, "a.go:12") || !strings.Contains(got, "leak") {
		t.Fatalf("expected file-scoped finding rendered, got: %q", got)
	}
	if !strings.Contains(got, "(general)") || !strings.Contains(got, "unspecified") {
		t.Fatalf("expected general/unspecified rendering, got: %q", got)
	}
	if strings.Count(got, "\n- ") != 2 {
		t.Fatalf("expected 2 rendered findings (blank skipped), got: %q", got)
	}
	// Each finding body is fenced and labeled as untrusted data (prompt-injection
	// hardening): a fence opener/closer per rendered finding plus the header note.
	if !strings.Contains(got, "untrusted") {
		t.Fatalf("expected the section to label finding text as untrusted, got: %q", got)
	}
	if strings.Count(got, "REVIEWER_TEXT") < 4 { // header note + (open+close) x2 findings
		t.Fatalf("expected each finding body fenced in a REVIEWER_TEXT block, got: %q", got)
	}

	// An over-long (or hostile) body is truncated so it cannot flood the prompt.
	long := strings.Repeat("A", devinFindingBodyMaxRunes+50)
	truncated := devinFindingsPromptSection([]scm.ReviewComment{{Path: "z.go", Line: 1, Severity: "high", Body: long}})
	if strings.Contains(truncated, long) {
		t.Fatalf("expected the long body to be truncated, but it was interpolated verbatim")
	}
	if !strings.Contains(truncated, "truncated") {
		t.Fatalf("expected a truncation marker, got: %q", truncated)
	}
}

// newReviewTestContext builds a minimal StepContext (no DB/git) sufficient for the
// review-loop integration points.
func newReviewTestContext(cfg config.ReviewLoop, headSHA string, log func(string)) *pipeline.StepContext {
	return &pipeline.StepContext{
		Ctx:    context.Background(),
		Run:    &db.Run{ID: "run-1", Branch: "refs/heads/feature", HeadSHA: headSHA},
		Config: &config.Config{ReviewLoop: cfg},
		Log:    log,
	}
}

func TestReviewLoopActive(t *testing.T) {
	t.Parallel()
	withReviews := &fakeReviewHost{caps: scm.Capabilities{Reviews: true}}
	noReviews := &fakeReviewHost{caps: scm.Capabilities{Reviews: false}}
	on := config.ReviewLoop{Enabled: true}
	off := config.ReviewLoop{Enabled: false}

	if reviewLoopActive(off, withReviews) {
		t.Error("disabled config must be inert even with review capability")
	}
	if reviewLoopActive(on, noReviews) {
		t.Error("enabled config must be inert without review capability")
	}
	if reviewLoopActive(on, nil) {
		t.Error("nil host must be inert")
	}
	if !reviewLoopActive(on, withReviews) {
		t.Error("enabled config + review capability must be active")
	}
}

func TestEvalDevinReview_VerdictToDecision(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, BotLogin: "my-bot", MaxRounds: 3, FailOpen: true}
	host := &fakeReviewHost{
		caps:     scm.Capabilities{Reviews: true},
		verdict:  scm.VerdictChangesRequested,
		findings: []scm.ReviewComment{severeFinding()},
	}
	sctx := newReviewTestContext(cfg, "headsha", func(string) {})
	pr := &scm.PR{Number: "42"}
	step := &CIStep{now: func() time.Time { return time.Unix(0, 0) }}

	decision, findings := step.evalDevinReview(sctx, host, pr)
	if decision != devinDecisionNotGreen {
		t.Fatalf("decision = %v, want notGreen", decision)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if host.gotPRNumber != 42 || host.gotHeadSHA != "headsha" || host.gotBotLogin != "my-bot" {
		t.Fatalf("host called with pr=%d head=%q bot=%q", host.gotPRNumber, host.gotHeadSHA, host.gotBotLogin)
	}
}

func TestEvalDevinReview_DefaultsBotLoginAndVerdictReadError(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, BotLogin: "", MaxRounds: 3, FailOpen: true}
	host := &fakeReviewHost{caps: scm.Capabilities{Reviews: true}, verdErr: context.DeadlineExceeded}
	var logs []string
	sctx := newReviewTestContext(cfg, "h1", func(s string) { logs = append(logs, s) })
	// Within grace, a read error is treated as not-yet-posted => pending.
	step := &CIStep{now: func() time.Time { return time.Unix(0, 0) }}
	decision, _ := step.evalDevinReview(sctx, host, &scm.PR{Number: "7"})
	if decision != devinDecisionPending {
		t.Fatalf("decision = %v, want pending on read error within grace", decision)
	}
	if host.gotBotLogin != config.DefaultReviewLoopBotLogin {
		t.Fatalf("expected default bot login, got %q", host.gotBotLogin)
	}
	// The loop now consumes findings from the verdict path, so it never makes a
	// separate GetBotFindings round-trip (least of all after a verdict read error).
	if host.findingsCalls != 0 {
		t.Fatalf("findings should not be fetched separately from the verdict, got %d calls", host.findingsCalls)
	}
}

func TestEvalDevinReview_GraceAnchorResetsOnHeadAdvance(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true}
	host := &fakeReviewHost{caps: scm.Capabilities{Reviews: true}, verdict: scm.VerdictNone}

	current := time.Unix(0, 0)
	step := &CIStep{now: func() time.Time { return current }}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})
	pr := &scm.PR{Number: "1"}

	// First sighting of headA at t=0 anchors the grace window.
	if d, _ := step.evalDevinReview(sctx, host, pr); d != devinDecisionPending {
		t.Fatalf("headA t=0: want pending, got %v", d)
	}
	// Past the grace window on headA => fail-open.
	current = current.Add(devinGraceWindow + time.Minute)
	if d, _ := step.evalDevinReview(sctx, host, pr); d != devinDecisionFailOpen {
		t.Fatalf("headA past grace: want failOpen, got %v", d)
	}
	// A fix advances the head; the anchor must reset so headB gets a fresh window.
	sctx.Run.HeadSHA = "headB"
	if d, _ := step.evalDevinReview(sctx, host, pr); d != devinDecisionPending {
		t.Fatalf("headB right after advance: want pending (fresh grace), got %v", d)
	}
	if step.devinAnchorSHA != "headB" || !step.devinAnchorAt.Equal(current) {
		t.Fatalf("anchor not reset: sha=%q at=%v now=%v", step.devinAnchorSHA, step.devinAnchorAt, current)
	}
}

// TestEvalDevinReview_StaleSevereFindingsOnUnreviewedHeadWaitsForReReview is the
// PR #5 multi-reply regression test. The scenario it locks down:
//
//  1. Devin reviewed headA and posted a severe finding → CHANGES_REQUESTED → the
//     loop fixed, pushed headB, posted "Addressed in <headB>" on the thread, and
//     re-triggered Devin for headB.
//  2. Next poll: head=headB. Devin has NOT reviewed headB yet, so GetReviewVerdict
//     returns PENDING. But GetBotFindings does NOT filter by head SHA, so the old
//     severe thread from headA is still returned (it is not resolved/outdated
//     because the fix touched different lines than the comment's anchor).
//
// On the buggy code, evalDevinGate treated PENDING + severe findings as NotGreen,
// so handleDevinFixRound ran again, pushed headC, and posted a SECOND
// "Addressed in <headC>" reply on the same thread — repeating up to MaxRounds
// (observed 7 replies across 7 commits on PR #5). The fix: a PENDING verdict
// means Devin has not weighed in on this head, so the loop must WAIT for the
// re-review (within the grace window) rather than re-fixing stale findings.
// CHANGES_REQUESTED is the only authoritative NotGreen signal and already
// encodes severe findings on the current head.
func TestEvalDevinReview_StaleSevereFindingsOnUnreviewedHeadWaitsForReReview(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true}
	// PENDING verdict (Devin reviewed an older head, not headB) with the stale
	// severe finding still live (not resolved/outdated) — exactly poll 2 above.
	host := &fakeReviewHost{
		caps:     scm.Capabilities{Reviews: true},
		verdict:  scm.VerdictPending,
		findings: []scm.ReviewComment{severeFinding()},
	}

	step := &CIStep{now: func() time.Time { return time.Unix(0, 0) }}
	sctx := newReviewTestContext(cfg, "headB", func(string) {})
	pr := &scm.PR{Number: "42"}

	decision, returnedFindings := step.evalDevinReview(sctx, host, pr)
	if decision != devinDecisionPending {
		t.Fatalf("decision = %v, want pending (wait for Devin to re-review headB; stale findings must not re-fix), findings=%v", decision, returnedFindings)
	}
	// The loop must not have recorded any fix round or reply for stale findings
	// on an unreviewed head — handleDevinFixRound is only called on NotGreen.
	if step.devinFixRounds != 0 {
		t.Fatalf("devinFixRounds = %d, want 0 (no fix round should run on PENDING)", step.devinFixRounds)
	}
	if len(host.replies) != 0 {
		t.Fatalf("replies = %v, want none (no re-fix → no acknowledgement on PENDING)", host.replies)
	}
}

func TestHandleDevinFixRound_EscalatesAtMaxRounds(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 2, FailOpen: true}
	var logs []string
	sctx := newReviewTestContext(cfg, "head", func(s string) { logs = append(logs, s) })
	step := &CIStep{devinFixRounds: 2} // already at the bound
	findings := []scm.ReviewComment{severeFinding()}

	outcome := step.handleDevinFixRound(sctx, &fakeReviewHost{}, &scm.PR{Number: "9"}, findings, "freshkey", nil, false)
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected escalation to human gate, got %+v", outcome)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "escalating for manual review") {
		t.Fatalf("expected escalation log, got: %v", logs)
	}
}

// TestHandleDevinFixRound_ForcedBypassesMaxRounds asserts that a user/agent
// `fix` response (forced=true) after the loop parked at MaxRounds runs one more
// fix round instead of immediately re-escalating on the same exhausted state.
func TestHandleDevinFixRound_ForcedBypassesMaxRounds(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 2, FailOpen: true}
	dir, baseSHA, headSHA, upstream := setupDevinFixRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "devin-fix.txt"), []byte("fixed"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.ReviewLoop = cfg
	sctx.Log = func(string) {}
	sctx.LogChunk = func(string) {}

	host := &fakeReviewHost{caps: scm.Capabilities{Reviews: true}}
	// Already at the bound and the key matches the last fixed round: an auto
	// (forced=false) round would escalate. A forced round must run anyway.
	step := &CIStep{devinFixRounds: 2, lastDevinFixKey: "freshkey"}
	findings := []scm.ReviewComment{severeFinding()}

	outcome := step.handleDevinFixRound(sctx, host, &scm.PR{Number: "42"}, findings, "freshkey", nil, true)
	if outcome != nil {
		t.Fatalf("forced fix must run, not escalate, got %+v", outcome)
	}
	if step.devinFixRounds != 3 {
		t.Fatalf("forced fix should have run one more round (devinFixRounds=%d, want 3)", step.devinFixRounds)
	}
	if sctx.Run.HeadSHA == headSHA {
		t.Fatalf("forced fix should have pushed a new head, still at %s", headSHA)
	}
}

func TestHandleDevinFixRound_WaitsWhenKeyAlreadyFixed(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true}
	var logs []string
	sctx := newReviewTestContext(cfg, "head", func(s string) { logs = append(logs, s) })
	// The Devin loop keys its already-fixed check on its own dedicated field, not
	// the shared lastFixedChecks (which the CI-check fix path uses).
	step := &CIStep{lastDevinFixKey: "samekey"}

	outcome := step.handleDevinFixRound(sctx, &fakeReviewHost{}, &scm.PR{Number: "9"}, nil, "samekey", nil, false)
	if outcome != nil {
		t.Fatalf("expected nil outcome (keep polling) when key already fixed, got %+v", outcome)
	}
	if step.devinFixRounds != 0 {
		t.Fatalf("expected no new fix round when waiting for re-review, got %d", step.devinFixRounds)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "re-reviewing") {
		t.Fatalf("expected re-reviewing log, got: %v", logs)
	}
}

// TestHandleDevinFixRound_TransientFixFailureDoesNotConsumeRound asserts that a
// fix attempt that errors (a transient network/API hiccup) does NOT permanently
// consume one of the bounded MaxRounds: the round is counted only after the fix
// attempt completes. Repeated transient failures must keep polling, not escalate.
func TestHandleDevinFixRound_TransientFixFailureDoesNotConsumeRound(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true}
	var logs []string
	sctx := newReviewTestContext(cfg, "head", func(s string) { logs = append(logs, s) })
	sctx.WorkDir = t.TempDir()
	sctx.Repo = &db.Repo{ID: "repo-1", DefaultBranch: "main", UpstreamURL: "https://github.com/test/repo"}
	sctx.Run.BaseSHA = "0000000000000000000000000000000000000000"
	sctx.LogChunk = func(string) {}
	// An agent that always errors models a transient hiccup mid-fix: autoFixCI
	// returns (false, err) before producing or pushing any change.
	sctx.Agent = &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("transient network error")
	}}
	host := &fakeReviewHost{caps: scm.Capabilities{Reviews: true}}
	findings := []scm.ReviewComment{severeFinding()}
	step := &CIStep{}

	// Far more iterations than MaxRounds: none should be consumed, none escalate.
	for i := 0; i < 5; i++ {
		outcome := step.handleDevinFixRound(sctx, host, &scm.PR{Number: "9"}, findings, "freshkey", nil, false)
		if outcome != nil {
			t.Fatalf("iteration %d: transient fix failure must keep polling, got escalation %+v", i, outcome)
		}
		if step.devinFixRounds != 0 {
			t.Fatalf("iteration %d: transient fix failure consumed a round (devinFixRounds=%d)", i, step.devinFixRounds)
		}
	}
	if !strings.Contains(strings.Join(logs, "\n"), "Devin fix failed") {
		t.Fatalf("expected a fix-failed warning log, got: %v", logs)
	}
}

// setupDevinFixRepo builds a working repo on `feature` plus a bare upstream so a
// review-loop fix round can actually commit and push (autoFixCI -> commitAndPush).
// Returns (workdir, baseSHA, headSHA, upstreamURL).
func setupDevinFixRepo(t *testing.T) (string, string, string, string) {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")
	return dir, baseSHA, headSHA, upstream
}

// devinFixRoundResult bundles the observable state after one handleDevinFixRound.
type devinFixRoundResult struct {
	step    *CIStep
	host    *fakeReviewHost
	sctx    *pipeline.StepContext
	outcome *pipeline.StepOutcome
}

// runDevinFixRound drives a single handleDevinFixRound end-to-end against a real
// git repo + bare upstream. produceChanges controls whether the fix agent writes a
// file (and thus whether autoFixCI actually pushes); replyErr is returned by every
// ReplyToReviewComment so the best-effort path can be exercised.
func runDevinFixRound(t *testing.T, cfg config.ReviewLoop, produceChanges bool, replyErr error, findings []scm.ReviewComment) devinFixRoundResult {
	t.Helper()
	dir, baseSHA, headSHA, upstream := setupDevinFixRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if produceChanges {
			if err := os.WriteFile(filepath.Join(opts.CWD, "devin-fix.txt"), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.ReviewLoop = cfg
	sctx.Log = func(string) {}
	sctx.LogChunk = func(string) {}

	host := &fakeReviewHost{caps: scm.Capabilities{Reviews: true}, replyErr: replyErr}
	step := &CIStep{}
	outcome := step.handleDevinFixRound(sctx, host, &scm.PR{Number: "42"}, findings, "freshkey", nil, false)
	return devinFixRoundResult{step: step, host: host, sctx: sctx, outcome: outcome}
}

// TestHandleDevinFixRound_RepliesPerAddressedFindingAfterPush asserts that after a
// successful fix push the loop posts one threaded reply per addressed finding that
// carries a comment id, skipping findings with ID==0.
func TestHandleDevinFixRound_RepliesPerAddressedFindingAfterPush(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true, ReplyOnFix: true}
	findings := []scm.ReviewComment{
		{ID: 111, Path: "a.go", Line: 1, Severity: "high", Body: "bug 1"},
		{ID: 222, Path: "b.go", Line: 2, Severity: "medium", Body: "bug 2"},
		{ID: 0, Path: "c.go", Line: 3, Severity: "high", Body: "no id, must be skipped"},
	}
	res := runDevinFixRound(t, cfg, true, nil, findings)

	if res.outcome != nil {
		t.Fatalf("expected nil outcome (keep polling), got %+v", res.outcome)
	}
	if res.step.devinFixRounds != 1 {
		t.Fatalf("devinFixRounds = %d, want 1", res.step.devinFixRounds)
	}
	if len(res.host.replies) != 2 {
		t.Fatalf("len(replies) = %d, want 2 (one per finding with an ID; ID==0 skipped): %+v", len(res.host.replies), res.host.replies)
	}
	wantBody := fmt.Sprintf("Addressed in %s by no-mistakes.", shortSHA(res.sctx.Run.HeadSHA))
	got := map[int64]bool{}
	for _, r := range res.host.replies {
		if r.prNumber != 42 {
			t.Errorf("reply prNumber = %d, want 42", r.prNumber)
		}
		if r.body != wantBody {
			t.Errorf("reply body = %q, want %q (short NEW head SHA)", r.body, wantBody)
		}
		got[r.commentID] = true
	}
	if !got[111] || !got[222] {
		t.Fatalf("expected replies on findings 111 and 222, got %v", got)
	}
	if got[0] {
		t.Fatal("a reply was posted for a finding with ID==0, which must be skipped")
	}
}

// TestHandleDevinFixRound_NoReplyWhenFixDidNotPush asserts no acknowledgement is
// posted when the fix agent produced no changes (no push).
func TestHandleDevinFixRound_NoReplyWhenFixDidNotPush(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true, ReplyOnFix: true}
	findings := []scm.ReviewComment{{ID: 111, Path: "a.go", Line: 1, Severity: "high", Body: "bug"}}
	res := runDevinFixRound(t, cfg, false, nil, findings)

	if res.outcome != nil {
		t.Fatalf("expected nil outcome, got %+v", res.outcome)
	}
	if res.step.devinFixRounds != 1 {
		t.Fatalf("devinFixRounds = %d, want 1 (the round is counted even on a clean no-op)", res.step.devinFixRounds)
	}
	if len(res.host.replies) != 0 {
		t.Fatalf("expected no replies when the fix did not push, got %+v", res.host.replies)
	}
}

// TestHandleDevinFixRound_NoReplyWhenReplyOnFixDisabled asserts the feature flag
// gates the acknowledgement even after a successful push.
func TestHandleDevinFixRound_NoReplyWhenReplyOnFixDisabled(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true, ReplyOnFix: false}
	findings := []scm.ReviewComment{{ID: 111, Path: "a.go", Line: 1, Severity: "high", Body: "bug"}}
	res := runDevinFixRound(t, cfg, true, nil, findings)

	if len(res.host.replies) != 0 {
		t.Fatalf("expected no replies when ReplyOnFix is false, got %+v", res.host.replies)
	}
}

// TestHandleDevinFixRound_NoReplyWhenLoopDisabled asserts a disabled review loop
// posts nothing even with ReplyOnFix set (keeps the disabled path inert).
func TestHandleDevinFixRound_NoReplyWhenLoopDisabled(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: false, MaxRounds: 3, FailOpen: true, ReplyOnFix: true}
	findings := []scm.ReviewComment{{ID: 111, Path: "a.go", Line: 1, Severity: "high", Body: "bug"}}
	res := runDevinFixRound(t, cfg, true, nil, findings)

	if len(res.host.replies) != 0 {
		t.Fatalf("expected no replies when the review loop is disabled, got %+v", res.host.replies)
	}
}

// TestHandleDevinFixRound_ReplyErrorDoesNotFailRound asserts the acknowledgement
// is best-effort: a reply error neither escalates the round nor prevents the fix
// key / round count from being recorded.
func TestHandleDevinFixRound_ReplyErrorDoesNotFailRound(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true, ReplyOnFix: true}
	findings := []scm.ReviewComment{
		{ID: 111, Path: "a.go", Line: 1, Severity: "high", Body: "bug 1"},
		{ID: 222, Path: "b.go", Line: 2, Severity: "medium", Body: "bug 2"},
	}
	res := runDevinFixRound(t, cfg, true, errors.New("reply boom"), findings)

	if res.outcome != nil {
		t.Fatalf("a reply error must not escalate the round, got %+v", res.outcome)
	}
	if res.step.devinFixRounds != 1 {
		t.Fatalf("devinFixRounds = %d, want 1 (round completes despite the reply error)", res.step.devinFixRounds)
	}
	if res.step.devinFixKey() != "freshkey" {
		t.Fatalf("fix key not recorded after a successful push: %q", res.step.devinFixKey())
	}
	if len(res.host.replies) != 2 {
		t.Fatalf("expected a reply attempt per finding even when they error, got %+v", res.host.replies)
	}
}

// TestCIStepDevinFieldsRaceSafe exercises the devinMu-guarded accessors
// concurrently so `go test -race` flags any unsynchronized access to the new
// Devin mutable fields.
func TestCIStepDevinFieldsRaceSafe(t *testing.T) {
	t.Parallel()
	step := &CIStep{}
	now := func() time.Time { return time.Unix(0, 0) }
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			step.devinAnchorReset("head", now)
			step.recordDevinRound()
			_ = step.devinRounds()
			step.claimRetrigger("head")
			_ = step.retriggeredSHA()
		}(i)
	}
	wg.Wait()
	if got := step.devinRounds(); got != 8 {
		t.Fatalf("recordDevinRound under contention = %d, want 8", got)
	}
	if got := step.retriggeredSHA(); got != "head" {
		t.Fatalf("retriggeredSHA under contention = %q, want head", got)
	}
}

// fakeLoopAPIKey is a fake, non-secret value used to exercise the re-trigger
// wiring. A real Devin API key must never appear in a test file.
const fakeLoopAPIKey = "loop-fake-key-NOT-REAL"

// recordingTrigger is an injected stand-in for devin.Client.TriggerReview so the
// loop tests never make a real HTTP call. It records each call's args and can be
// made to fail.
type recordingTrigger struct {
	mu    sync.Mutex
	calls []triggerCall
	err   error // when set, every call returns this error
}

type triggerCall struct {
	apiKey  string
	prURL   string
	headSHA string
}

func (r *recordingTrigger) fn(_ context.Context, apiKey, prURL, headSHA string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, triggerCall{apiKey: apiKey, prURL: prURL, headSHA: headSHA})
	if r.err != nil {
		return "", r.err
	}
	return "sess-" + headSHA, nil
}

func (r *recordingTrigger) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingTrigger) last() triggerCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

// TestMaybeRetriggerDevin_OncePerHeadAcrossPolls asserts the cost-critical guard:
// across many polls of the same head, the loop triggers Devin exactly once and
// passes the resolved key + PR URL + head SHA.
func TestMaybeRetriggerDevin_OncePerHeadAcrossPolls(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", fakeLoopAPIKey) // env precedence -> no file/network
	cfg := config.ReviewLoop{Enabled: true, Retrigger: true}
	rt := &recordingTrigger{}
	step := &CIStep{triggerReview: rt.fn}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})
	pr := &scm.PR{Number: "1", URL: "https://github.com/o/r/pull/1"}

	for i := 0; i < 5; i++ {
		step.maybeRetriggerDevin(sctx, pr, "headA")
	}

	if rt.count() != 1 {
		t.Fatalf("trigger count = %d, want 1 (once per head across polls)", rt.count())
	}
	got := rt.last()
	if got.headSHA != "headA" || got.prURL != pr.URL || got.apiKey != fakeLoopAPIKey {
		t.Fatalf("trigger args = %+v, want headA + pr url + resolved key", got)
	}
	if step.retriggeredSHA() != "headA" {
		t.Fatalf("lastRetriggeredSHA = %q, want headA", step.retriggeredSHA())
	}
}

// TestMaybeRetriggerDevin_TriggersAgainOnNewHead asserts a new head SHA (e.g.
// after a fix push) triggers a fresh review while a repeat of the same head does
// not.
func TestMaybeRetriggerDevin_TriggersAgainOnNewHead(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", fakeLoopAPIKey)
	cfg := config.ReviewLoop{Enabled: true, Retrigger: true}
	rt := &recordingTrigger{}
	step := &CIStep{triggerReview: rt.fn}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})
	pr := &scm.PR{Number: "1", URL: "https://github.com/o/r/pull/1"}

	step.maybeRetriggerDevin(sctx, pr, "headA")
	step.maybeRetriggerDevin(sctx, pr, "headA") // duplicate -> no-op
	step.maybeRetriggerDevin(sctx, pr, "headB") // new head -> triggers again

	if rt.count() != 2 {
		t.Fatalf("trigger count = %d, want 2 (headA + headB, dup suppressed)", rt.count())
	}
	if got := rt.last(); got.headSHA != "headB" {
		t.Fatalf("second trigger head = %q, want headB", got.headSHA)
	}
}

func TestMaybeRetriggerDevin_NoopWhenLoopDisabled(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", fakeLoopAPIKey)
	cfg := config.ReviewLoop{Enabled: false, Retrigger: true}
	rt := &recordingTrigger{}
	step := &CIStep{triggerReview: rt.fn}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})

	step.maybeRetriggerDevin(sctx, &scm.PR{Number: "1", URL: "u"}, "headA")

	if rt.count() != 0 {
		t.Fatalf("trigger count = %d, want 0 when the loop is disabled", rt.count())
	}
	if step.retriggeredSHA() != "" {
		t.Fatalf("guard consumed when the loop is disabled: %q", step.retriggeredSHA())
	}
}

// TestMaybeRetriggerDevin_PrefersReviewAPIWhenConfigured asserts that when a
// Devin Review token AND org id are configured, the loop uses the dedicated
// Review API (POST /v3/.../pr-reviews) and NOT the legacy /v1/sessions path.
func TestMaybeRetriggerDevin_PrefersReviewAPIWhenConfigured(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", fakeLoopAPIKey)          // legacy key present
	t.Setenv("DEVIN_REVIEW_API_KEY", "cog-review-key") // review token present (env precedence)
	cfg := config.ReviewLoop{Enabled: true, Retrigger: true, DevinOrgID: "org-42"}

	var prCalls, sessionCalls int
	var gotToken, gotOrg, gotURL string
	step := &CIStep{
		triggerPRReview: func(_ context.Context, token, orgID, prURL string) (string, error) {
			prCalls++
			gotToken, gotOrg, gotURL = token, orgID, prURL
			return "pending", nil
		},
		triggerReview: func(context.Context, string, string, string) (string, error) {
			sessionCalls++
			return "sess", nil
		},
	}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})
	pr := &scm.PR{Number: "1", URL: "https://github.com/o/r/pull/1"}

	step.maybeRetriggerDevin(sctx, pr, "headA")

	if prCalls != 1 {
		t.Fatalf("Review-API trigger count = %d, want 1", prCalls)
	}
	if sessionCalls != 0 {
		t.Fatalf("legacy /v1/sessions trigger count = %d, want 0 (Review API preferred)", sessionCalls)
	}
	if gotToken != "cog-review-key" || gotOrg != "org-42" || gotURL != pr.URL {
		t.Fatalf("review args = (%q,%q,%q), want (cog-review-key, org-42, %q)", gotToken, gotOrg, gotURL, pr.URL)
	}
}

// TestMaybeRetriggerDevin_FallsBackToSessionsWithoutReviewToken asserts that
// without a review token (even with an org id set) the loop stays on the legacy
// /v1/sessions trigger, preserving existing behavior for unconfigured orgs.
func TestMaybeRetriggerDevin_FallsBackToSessionsWithoutReviewToken(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", fakeLoopAPIKey)
	t.Setenv("DEVIN_REVIEW_API_KEY", "") // no review token
	cfg := config.ReviewLoop{
		Enabled:               true,
		Retrigger:             true,
		DevinOrgID:            "org-42",
		DevinReviewAPIKeyFile: "/nonexistent/no-mistakes-test-review-key", // absent -> resolves to ""
	}
	rt := &recordingTrigger{}
	prCalls := 0
	step := &CIStep{
		triggerReview:   rt.fn,
		triggerPRReview: func(context.Context, string, string, string) (string, error) { prCalls++; return "pending", nil },
	}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})
	pr := &scm.PR{Number: "1", URL: "https://github.com/o/r/pull/1"}

	step.maybeRetriggerDevin(sctx, pr, "headA")

	if prCalls != 0 {
		t.Fatalf("Review-API trigger count = %d, want 0 (no review token)", prCalls)
	}
	if rt.count() != 1 {
		t.Fatalf("legacy /v1/sessions trigger count = %d, want 1 (fallback)", rt.count())
	}
}

func TestMaybeRetriggerDevin_NoopWhenRetriggerFalse(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", fakeLoopAPIKey)
	cfg := config.ReviewLoop{Enabled: true, Retrigger: false}
	rt := &recordingTrigger{}
	step := &CIStep{triggerReview: rt.fn}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})

	step.maybeRetriggerDevin(sctx, &scm.PR{Number: "1", URL: "u"}, "headA")

	if rt.count() != 0 {
		t.Fatalf("trigger count = %d, want 0 when Retrigger=false", rt.count())
	}
	if step.retriggeredSHA() != "" {
		t.Fatalf("guard consumed when Retrigger=false: %q", step.retriggeredSHA())
	}
}

// TestMaybeRetriggerDevin_NoopWhenNoKey asserts a missing key skips the trigger
// WITHOUT consuming the once-per-head guard, so a key that appears later can still
// trigger the same head.
func TestMaybeRetriggerDevin_NoopWhenNoKey(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", "") // env empty
	dir := t.TempDir()
	cfg := config.ReviewLoop{
		Enabled:         true,
		Retrigger:       true,
		DevinAPIKeyFile: filepath.Join(dir, "absent"), // no such file -> no key
	}
	rt := &recordingTrigger{}
	step := &CIStep{triggerReview: rt.fn}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})

	step.maybeRetriggerDevin(sctx, &scm.PR{Number: "1", URL: "u"}, "headA")

	if rt.count() != 0 {
		t.Fatalf("trigger count = %d, want 0 when no key resolves", rt.count())
	}
	if step.retriggeredSHA() != "" {
		t.Fatalf("guard consumed when no key resolved: %q (a later key must still trigger)", step.retriggeredSHA())
	}
}

// TestMaybeRetriggerDevin_ErrorIsSwallowedAndNotRetried asserts a trigger error
// (e.g. rate-limited) is best-effort: it is attempted at most once per head (the
// head is claimed BEFORE the call) and never panics or changes control flow.
func TestMaybeRetriggerDevin_ErrorIsSwallowedAndNotRetried(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", fakeLoopAPIKey)
	cfg := config.ReviewLoop{Enabled: true, Retrigger: true}
	rt := &recordingTrigger{err: errors.New("rate limited")}
	step := &CIStep{triggerReview: rt.fn}
	sctx := newReviewTestContext(cfg, "headA", func(string) {})
	pr := &scm.PR{Number: "1", URL: "u"}

	for i := 0; i < 3; i++ {
		step.maybeRetriggerDevin(sctx, pr, "headA")
	}

	if rt.count() != 1 {
		t.Fatalf("trigger count = %d, want 1 (error path claims the head; no per-poll retry)", rt.count())
	}
	if step.retriggeredSHA() != "headA" {
		t.Fatalf("guard not set after an attempted (failed) trigger: %q", step.retriggeredSHA())
	}
}

// runRetriggerFixRound drives a real handleDevinFixRound (with a fix that pushes a
// new commit) with retrigger enabled and an injected trigger, returning the
// observable state plus the pre-fix head SHA.
func runRetriggerFixRound(t *testing.T, triggerErr error) (*CIStep, *pipeline.StepContext, *recordingTrigger, *pipeline.StepOutcome, string) {
	t.Helper()
	t.Setenv("DEVIN_API_KEY", fakeLoopAPIKey)
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true, ReplyOnFix: false, Retrigger: true}
	dir, baseSHA, headSHA, upstream := setupDevinFixRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "devin-fix.txt"), []byte("fixed"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.ReviewLoop = cfg
	sctx.Log = func(string) {}
	sctx.LogChunk = func(string) {}

	rt := &recordingTrigger{err: triggerErr}
	host := &fakeReviewHost{caps: scm.Capabilities{Reviews: true}}
	step := &CIStep{triggerReview: rt.fn}
	pr := &scm.PR{Number: "42", URL: "https://github.com/o/r/pull/42"}
	outcome := step.handleDevinFixRound(sctx, host, pr, []scm.ReviewComment{severeFinding()}, "freshkey", nil, false)
	return step, sctx, rt, outcome, headSHA
}

// TestHandleDevinFixRound_RetriggersDevinOnNewHeadAfterPush asserts that after a
// successful fix push the loop re-triggers Devin on the NEW head SHA.
func TestHandleDevinFixRound_RetriggersDevinOnNewHeadAfterPush(t *testing.T) {
	step, sctx, rt, outcome, oldHead := runRetriggerFixRound(t, nil)

	if outcome != nil {
		t.Fatalf("expected nil outcome (keep polling), got %+v", outcome)
	}
	if step.devinFixRounds != 1 {
		t.Fatalf("devinFixRounds = %d, want 1", step.devinFixRounds)
	}
	if rt.count() != 1 {
		t.Fatalf("trigger count = %d, want 1 after a fix push", rt.count())
	}
	got := rt.last()
	if got.headSHA != sctx.Run.HeadSHA {
		t.Fatalf("retrigger head = %q, want the new pushed head %q", got.headSHA, sctx.Run.HeadSHA)
	}
	if got.headSHA == oldHead {
		t.Fatal("retrigger used the OLD head; expected the new pushed head")
	}
}

// TestHandleDevinFixRound_RetriggerErrorDoesNotFailRound asserts a re-trigger
// error after a fix push is best-effort: the round still completes (nil outcome,
// round counted, fix key recorded).
func TestHandleDevinFixRound_RetriggerErrorDoesNotFailRound(t *testing.T) {
	step, _, rt, outcome, _ := runRetriggerFixRound(t, errors.New("trigger boom"))

	if outcome != nil {
		t.Fatalf("a re-trigger error must not escalate the round, got %+v", outcome)
	}
	if step.devinFixRounds != 1 {
		t.Fatalf("devinFixRounds = %d, want 1 (round completes despite the trigger error)", step.devinFixRounds)
	}
	if step.devinFixKey() != "freshkey" {
		t.Fatalf("fix key not recorded after a successful push: %q", step.devinFixKey())
	}
	if rt.count() != 1 {
		t.Fatalf("trigger count = %d, want 1 attempt even though it errored", rt.count())
	}
}
