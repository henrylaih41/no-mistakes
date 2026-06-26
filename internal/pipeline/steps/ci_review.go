package steps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// devinGraceWindow is how long the post-PR review loop waits for the review bot
// (Devin) to post a verdict on a given head SHA before treating its silence as a
// final state (fail-open green, or — when fail-closed — continued waiting until
// the CI idle timeout escalates). The anchor resets on every new commit, so each
// fix push gets a fresh window for the bot to re-review.
const devinGraceWindow = 11 * time.Minute

// devinGateDecision is the outcome of evaluating the review bot's gate for the
// current head SHA. It is the single decision the CI loop consumes.
type devinGateDecision int

const (
	// devinDecisionDisabled means the review loop is off (or the host has no
	// review capability); the gate has no effect and behavior is unchanged.
	devinDecisionDisabled devinGateDecision = iota
	// devinDecisionPending means the bot has not yet posted a verdict on the
	// current head SHA (still within the grace window, or fail-closed and
	// silent). The PR is not ready and no fix should be attempted yet.
	devinDecisionPending
	// devinDecisionNotGreen means the bot requested changes / left unresolved
	// severe findings on the current head SHA; no-mistakes should fix them.
	devinDecisionNotGreen
	// devinDecisionGreen means the bot reviewed the current head SHA and left no
	// unresolved severe findings.
	devinDecisionGreen
	// devinDecisionFailOpen means the bot stayed silent past the grace window and
	// fail-open is configured: proceed on checks-only green with a loud log.
	devinDecisionFailOpen
)

// evalDevinGate maps a review verdict + findings + loop config + elapsed time
// since the current head SHA was first seen into a single loop decision. It is a
// pure function (no I/O, no clock) so it is exhaustively unit-testable; all host
// access lives in evalDevinReview.
//
// "Not green" is verdict==CHANGES_REQUESTED OR any unresolved severe file-scoped
// finding on the head SHA (the verdict already encodes this, but the findings
// are checked directly so a verdict that rolled up to PENDING but still carries
// a severe finding is not mistaken for "no findings"). A PENDING/NONE verdict
// with no severe findings means the bot has not weighed in on this head yet:
// within the grace window that is "pending"; past it, fail-open turns it into a
// checks-only green and fail-closed keeps it pending.
func evalDevinGate(verdict scm.ReviewVerdict, findings []scm.ReviewComment, cfg config.ReviewLoop, elapsed time.Duration) devinGateDecision {
	if !cfg.Enabled {
		return devinDecisionDisabled
	}
	if verdict == scm.VerdictChangesRequested || hasUnresolvedSevereFindings(findings) {
		return devinDecisionNotGreen
	}
	if verdict == scm.VerdictApproved {
		return devinDecisionGreen
	}
	// PENDING or NONE with no severe findings: the bot has not reviewed this head.
	if elapsed < devinGraceWindow {
		return devinDecisionPending
	}
	if cfg.FailOpen {
		return devinDecisionFailOpen
	}
	// Fail-closed: keep waiting. The CI step's idle timeout eventually escalates
	// to the human gate rather than silently merging without a review.
	return devinDecisionPending
}

// hasUnresolvedSevereFindings reports whether any finding is a severe
// (high/medium) file-scoped comment. It mirrors the verdict derivation in the
// github read layer: top-level summary comments (no Path) are informational.
func hasUnresolvedSevereFindings(findings []scm.ReviewComment) bool {
	for _, f := range findings {
		if f.Path == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(f.Severity)) {
		case "high", "medium":
			return true
		}
	}
	return false
}

// devinFindingFingerprints returns a stable, sorted set of fingerprints for the
// bot's findings. Folded into the anti-thrash key, it lets the loop tell "the
// same findings on the same commit" (wait for re-review) apart from "new
// findings, or the same findings on a new commit" (fix again).
func devinFindingFingerprints(findings []scm.ReviewComment) []string {
	if len(findings) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(findings))
	prints := make([]string, 0, len(findings))
	for _, f := range findings {
		raw := fmt.Sprintf("%s\x00%d\x00%s\x00%s", f.Path, f.Line, f.Severity, strings.TrimSpace(f.Body))
		sum := sha256.Sum256([]byte(raw))
		fp := hex.EncodeToString(sum[:8])
		if _, ok := seen[fp]; ok {
			continue
		}
		seen[fp] = struct{}{}
		prints = append(prints, fp)
	}
	sort.Strings(prints)
	return prints
}

// devinFindingsPromptSection renders the review bot's findings as a prompt
// section appended to the CI fix prompt. Returns "" when there are no findings,
// so the fix prompt is unchanged whenever the review loop is inactive.
func devinFindingsPromptSection(findings []scm.ReviewComment) string {
	var lines []string
	for _, f := range findings {
		body := strings.TrimSpace(f.Body)
		if body == "" {
			continue
		}
		severity := strings.TrimSpace(f.Severity)
		if severity == "" {
			severity = "unspecified"
		}
		loc := "(general)"
		if f.Path != "" {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		lines = append(lines, fmt.Sprintf("- [%s] %s\n  %s", severity, loc, body))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\nReview bot (Devin) findings to fix:\n" + strings.Join(lines, "\n")
}

// reviewLoopActive reports whether the post-PR review loop should run: it is
// gated on both the config flag and the host's review capability, so a host that
// cannot read reviews never changes behavior even with the loop enabled.
func reviewLoopActive(cfg config.ReviewLoop, host scm.Host) bool {
	return cfg.Enabled && host != nil && host.Capabilities().Reviews
}

// evalDevinReview reads the review bot's verdict + findings for the current head
// SHA and reduces them to a gate decision. This is the single integration point
// where the CI loop consumes scm.Host's review methods, so tests mock the host
// here. It also maintains the per-head grace anchor (reset whenever the head
// advances) that evalDevinGate consumes as elapsed time.
func (s *CIStep) evalDevinReview(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) (devinGateDecision, []scm.ReviewComment) {
	ctx := sctx.Ctx
	cfg := sctx.Config.ReviewLoop
	headSHA := sctx.Run.HeadSHA

	prNum, err := strconv.Atoi(pr.Number)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: cannot parse PR number %q for Devin review: %v", pr.Number, err))
		return devinDecisionDisabled, nil
	}
	botLogin := strings.TrimSpace(cfg.BotLogin)
	if botLogin == "" {
		botLogin = config.DefaultReviewLoopBotLogin
	}

	now := s.now
	if now == nil {
		now = time.Now
	}
	// Reset the grace anchor whenever the head advances so each new commit gets a
	// fresh window for the bot to (re-)review.
	if headSHA != s.devinAnchorSHA {
		s.devinAnchorSHA = headSHA
		s.devinAnchorAt = now()
	}

	// GetReviewVerdict returns the findings it already read to derive the verdict,
	// so the loop consumes both from one host round-trip instead of re-fetching.
	verdict, verdictFindings, err := host.GetReviewVerdict(ctx, prNum, headSHA, botLogin)
	if err != nil {
		// Treat a read error like "not yet posted": the grace window + fail
		// policy decide whether to wait, fail open, or hold.
		sctx.Log(fmt.Sprintf("warning: could not read Devin review verdict: %v", err))
		verdict = scm.VerdictNone
		verdictFindings = nil
	}

	// Only surface findings for the states that act on them (changes-requested or
	// a re-review still pending on this head); an approved/none verdict carries no
	// actionable findings, matching the prior two-call behavior.
	var findings []scm.ReviewComment
	if verdict == scm.VerdictChangesRequested || verdict == scm.VerdictPending {
		findings = verdictFindings
	}

	elapsed := now().Sub(s.devinAnchorAt)
	return evalDevinGate(verdict, findings, cfg, elapsed), findings
}

// handleDevinFixRound drives one bounded review-loop fix round for the case where
// the bot requested changes but CI checks are otherwise clean (no failing checks,
// no merge conflict). It returns a non-nil outcome to escalate to the human gate,
// or nil to keep polling. Rounds are bounded by ReviewLoop.MaxRounds; the
// anti-thrash key (fixKey) folds in the head SHA + finding fingerprints so a
// pushed fix waits for the bot to re-review the new commit before re-evaluating.
func (s *CIStep) handleDevinFixRound(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, findings []scm.ReviewComment, fixKey string, fixCompletedAt map[string]time.Time) *pipeline.StepOutcome {
	if fixKey != "" && fixKey == s.lastFixedChecks {
		sctx.Log(cimonitor.ReReviewingMsg)
		return nil
	}
	maxRounds := sctx.Config.ReviewLoop.MaxRounds
	if s.devinFixRounds >= maxRounds {
		sctx.Log(fmt.Sprintf("Devin still requesting changes after %d/%d round(s) - escalating for manual review...", s.devinFixRounds, maxRounds))
		return devinFailureOutcome(findings, "Devin review loop exhausted its bounded rounds with unresolved findings")
	}
	s.devinFixRounds++
	sctx.Log(fmt.Sprintf("%s - auto-fixing (round %d/%d)...", cimonitor.ReviewChangesRequestedMsg, s.devinFixRounds, maxRounds))
	previousHeadSHA := sctx.Run.HeadSHA
	pushed, err := s.autoFixCI(sctx, host, pr, nil, false, findings)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: Devin fix failed: %v", err))
		return nil
	}
	if pushed || sctx.Run.HeadSHA != previousHeadSHA {
		s.lastFixedChecks = fixKey
		s.lastFixedCompletedAt = fixCompletedAt
		sctx.Log(cimonitor.ReReviewingMsg)
	} else {
		// No changes produced: don't record the key so a later round can retry
		// until MaxRounds is reached, then escalate.
		sctx.Log("Devin fix produced no changes, will retry if rounds remain...")
	}
	return nil
}
