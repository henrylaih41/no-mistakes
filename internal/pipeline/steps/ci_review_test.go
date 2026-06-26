package steps

import (
	"context"
	"strings"
	"testing"
	"time"

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

	verdictCalls  int
	findingsCalls int
	gotHeadSHA    string
	gotBotLogin   string
	gotPRNumber   int
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
		{"severe finding overrides pending verdict", scm.VerdictPending, []scm.ReviewComment{severeFinding()}, enabled, time.Minute, devinDecisionNotGreen},
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
	// A changed body produces a different fingerprint set.
	changed := []scm.ReviewComment{{Path: "a.go", Line: 1, Severity: "high", Body: "x2"}, a[1]}
	if strings.Join(devinFindingFingerprints(a), ",") == strings.Join(devinFindingFingerprints(changed), ",") {
		t.Fatal("expected different fingerprints when a finding body changes")
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

func TestHandleDevinFixRound_EscalatesAtMaxRounds(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 2, FailOpen: true}
	var logs []string
	sctx := newReviewTestContext(cfg, "head", func(s string) { logs = append(logs, s) })
	step := &CIStep{devinFixRounds: 2} // already at the bound
	findings := []scm.ReviewComment{severeFinding()}

	outcome := step.handleDevinFixRound(sctx, &fakeReviewHost{}, &scm.PR{Number: "9"}, findings, "freshkey", nil)
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected escalation to human gate, got %+v", outcome)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "escalating for manual review") {
		t.Fatalf("expected escalation log, got: %v", logs)
	}
}

func TestHandleDevinFixRound_WaitsWhenKeyAlreadyFixed(t *testing.T) {
	t.Parallel()
	cfg := config.ReviewLoop{Enabled: true, MaxRounds: 3, FailOpen: true}
	var logs []string
	sctx := newReviewTestContext(cfg, "head", func(s string) { logs = append(logs, s) })
	step := &CIStep{lastFixedChecks: "samekey"}

	outcome := step.handleDevinFixRound(sctx, &fakeReviewHost{}, &scm.PR{Number: "9"}, nil, "samekey", nil)
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
