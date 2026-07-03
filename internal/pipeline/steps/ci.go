package steps

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	defaultChecksGracePeriod          = 60 * time.Second
	defaultBaseBranchTipResolveWindow = 30 * time.Second
)

// CI monitoring status messages. These are surfaced to the user and parsed by
// the TUI and the agent-facing axi commands to distinguish passed checks from
// checks that are still running. The canonical strings live in cimonitor so all
// producers and consumers agree on them.
const (
	ciChecksPassedMsg   = cimonitor.ChecksPassedMsg
	ciNoChecksPassedMsg = cimonitor.NoChecksPassedMsg
	ciChecksRunningMsg  = cimonitor.ChecksRunningMsg
)

// CIStep monitors an open PR until it is merged, closed, or its configured idle
// timeout elapses, auto-fixing CI failures.
type CIStep struct {
	// lastFixedChecks, lastFixedCompletedAt, and ciFixAttempts are written only by
	// the single CIStep.Execute goroutine (one CIStep per run). The Devin
	// review-loop fields below share that same single-writer assumption but are
	// additionally guarded by devinMu per AGENTS.md, since this PR introduced them
	// and their access spans helper methods that a future caller could reach off
	// the Execute goroutine.
	lastFixedChecks      string               // sorted check names from last fix attempt, to avoid re-fixing
	lastFixedCompletedAt map[string]time.Time // failing check completion times seen before the last fix attempt
	ciFixAttempts        int                  // number of CI auto-fix attempts made

	// devinMu guards the post-PR review-loop (Devin) mutable fields below. Reach
	// them only through the devinRounds/recordDevinRound/devinAnchorReset/
	// devinFixKey/recordDevinFixKey helpers.
	devinMu        sync.Mutex
	devinFixRounds int       // number of post-PR review-loop (Devin) fix rounds made
	devinAnchorSHA string    // head SHA the Devin grace window is anchored to
	devinAnchorAt  time.Time // when the current head SHA was first seen (Devin grace start)
	// lastDevinFixKey is the anti-thrash key for the last completed Devin fix round
	// — (headSHA, finding fingerprints). It is dedicated to the review loop rather
	// than reusing lastFixedChecks (the CI-check fix path's key) so the two paths
	// cannot collide on each other's keys.
	lastDevinFixKey string
	// lastRetriggeredSHA is the head SHA for which we last (attempted to) explicitly
	// trigger a Devin review via the API. Guarded by devinMu. It makes the trigger
	// idempotent at most once per head SHA: COST-CRITICAL, since the CI loop polls
	// every few seconds and each trigger creates a paid Devin session.
	lastRetriggeredSHA string

	// triggerReview, when nil, defaults to a real Devin API client. It is injected
	// in tests so the loop never makes a real HTTP call. Its signature matches
	// devin.Client.TriggerReview: (ctx, apiKey, prURL, headSHA) -> (sessionID, err).
	triggerReview func(ctx context.Context, apiKey, prURL, headSHA string) (string, error)

	// triggerPRReview, when nil, defaults to a real Devin API client. Injected in
	// tests. Its signature matches devin.Client.TriggerPRReview:
	// (ctx, token, orgID, prURL) -> (status, commitSHA, err). Used in preference to
	// triggerReview when a Devin Review token AND org id are configured.
	triggerPRReview func(ctx context.Context, token, orgID, prURL string) (string, string, error)

	checksGracePeriod    time.Duration // minimum wait before trusting empty CI checks (0 = default 60s)
	pollIntervalOverride time.Duration // if set, overrides computed poll interval (for testing)
	waitForNextPoll      func(context.Context, time.Duration) error
	now                  func() time.Time
	// baseBranchTip resolves the current tip SHA of the upstream default
	// branch. The bool is false when the SHA is a fallback/unknown value and
	// must not re-arm the timeout. Overridable for testing; defaults to
	// fetching the upstream default branch.
	baseBranchTip func(context.Context) (string, bool)
}

func (s *CIStep) Name() types.StepName { return types.StepCI }

func (s *CIStep) gracePeriod() time.Duration {
	if s.checksGracePeriod > 0 {
		return s.checksGracePeriod
	}
	return defaultChecksGracePeriod
}

func ciPRClosedAutoResolver(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) func(context.Context) bool {
	return func(ctx context.Context) bool {
		state, err := host.GetPRState(ctx, pr)
		if err != nil {
			if ctx.Err() == nil && sctx.Log != nil {
				sctx.Log(fmt.Sprintf("warning: could not re-check PR state while CI gate is parked: %v", err))
			}
			return false
		}
		switch state {
		case scm.PRStateMerged:
			if sctx.Log != nil {
				sctx.Log("PR has been merged after CI timeout; clearing parked CI gate")
			}
			return true
		case scm.PRStateClosed:
			if sctx.Log != nil {
				sctx.Log("PR has been closed after CI timeout; clearing parked CI gate")
			}
			return true
		default:
			return false
		}
	}
}

type reviewLoopActivation struct {
	active bool
	reason string
}

func reviewLoopForRun(sctx *pipeline.StepContext, host scm.Host) reviewLoopActivation {
	if sctx == nil || sctx.Config == nil {
		return reviewLoopActivation{}
	}
	cfg := sctx.Config.ReviewLoop
	if sctx.Run != nil && sctx.Run.ReviewLoopDisabled {
		return reviewLoopActivation{reason: "review loop disabled for this run via no-mistakes.review-loop=off; CI monitoring continues without Devin"}
	}
	if !reviewLoopActive(cfg, host) {
		return reviewLoopActivation{}
	}
	if reason := reviewLoopApplicabilitySkipReason(sctx.Repo); reason != "" {
		return reviewLoopActivation{reason: reason}
	}
	return reviewLoopActivation{active: true}
}

func reviewLoopApplicabilitySkipReason(repo *db.Repo) string {
	if repo == nil || strings.TrimSpace(repo.ForkURL) == "" {
		return ""
	}
	baseSlug := github.RepoSlug(repo.UpstreamURL)
	forkSlug := github.RepoSlug(repo.ForkURL)
	if baseSlug == "" || forkSlug == "" {
		return ""
	}
	baseOwner, _, baseOK := strings.Cut(baseSlug, "/")
	forkOwner, _, forkOK := strings.Cut(forkSlug, "/")
	if !baseOK || !forkOK || baseOwner == "" || forkOwner == "" {
		return ""
	}
	if strings.EqualFold(baseOwner, forkOwner) {
		return ""
	}
	return fmt.Sprintf("review loop skipped: PR base %s differs from fork %s; CI monitoring continues without Devin", baseSlug, forkSlug)
}

// devinRounds returns the number of completed post-PR review-loop fix rounds,
// read under devinMu. See CIStep for the single-writer note.
func (s *CIStep) devinRounds() int {
	s.devinMu.Lock()
	defer s.devinMu.Unlock()
	return s.devinFixRounds
}

// recordDevinRound counts a completed review-loop fix round (a push or a clean
// no-op) and returns the new total, written under devinMu. A round is recorded
// only after the fix attempt completes, so a transient failure does not consume
// one.
func (s *CIStep) recordDevinRound() int {
	s.devinMu.Lock()
	defer s.devinMu.Unlock()
	s.devinFixRounds++
	return s.devinFixRounds
}

// devinAnchorReset resets the Devin grace anchor whenever the head advances (so
// each new commit gets a fresh window for the bot to re-review) and returns the
// anchor time for the current head. All anchor reads/writes happen under devinMu.
func (s *CIStep) devinAnchorReset(headSHA string, now func() time.Time) time.Time {
	s.devinMu.Lock()
	defer s.devinMu.Unlock()
	if headSHA != s.devinAnchorSHA {
		s.devinAnchorSHA = headSHA
		s.devinAnchorAt = now()
	}
	return s.devinAnchorAt
}

// devinFixKey returns the anti-thrash key recorded for the last completed Devin
// fix round, read under devinMu. See CIStep for the single-writer note.
func (s *CIStep) devinFixKey() string {
	s.devinMu.Lock()
	defer s.devinMu.Unlock()
	return s.lastDevinFixKey
}

// recordDevinFixKey stores the anti-thrash key for the Devin fix round just
// pushed, written under devinMu. Dedicated to the review loop so it never
// collides with the CI-check path's lastFixedChecks.
func (s *CIStep) recordDevinFixKey(key string) {
	s.devinMu.Lock()
	defer s.devinMu.Unlock()
	s.lastDevinFixKey = key
}

// ciFixAlreadyAttempted reports whether the shared CI-fix path already pushed a
// fix for the current issue state, so the monitor should wait for CI to re-run
// rather than re-running the fixer. The CI-check identity (failing names + merge
// conflict, ciKey) and the review-loop identity (head SHA + finding
// fingerprints, devinKey) are compared as INDEPENDENT dimensions: the stable CI
// anti-thrash must not be reset just because the review verdict transitions from
// "not green" to "pending" on a freshly pushed head (devinKey would otherwise
// vanish from a conflated key and spuriously mismatch). A still-flagging review
// only re-triggers when it carries a distinct fingerprint set; with no
// fingerprints there is nothing new to feed the fixer, so the CI key alone gates.
func (s *CIStep) ciFixAlreadyAttempted(ciKey, devinKey string, devinNotGreen bool) bool {
	if ciKey == "" || ciKey != s.lastFixedChecks {
		return false
	}
	if !devinNotGreen || devinKey == "" {
		return true
	}
	return devinKey == s.devinFixKey()
}

// recordCIFix records the anti-thrash keys for a fix the shared CI-fix path just
// pushed. The CI key and (when the review loop flagged the head) the review key
// are stored separately so each dimension's anti-thrash survives the other's
// state changing on the next poll.
func (s *CIStep) recordCIFix(ciKey, devinKey string, devinNotGreen bool, fixCompletedAt map[string]time.Time) {
	s.lastFixedChecks = ciKey
	if devinNotGreen && devinKey != "" {
		s.recordDevinFixKey(devinKey)
	}
	s.lastFixedCompletedAt = fixCompletedAt
}

// retriggeredSHA returns the head SHA the last explicit Devin re-trigger was made
// for, read under devinMu.
func (s *CIStep) retriggeredSHA() string {
	s.devinMu.Lock()
	defer s.devinMu.Unlock()
	return s.lastRetriggeredSHA
}

// claimRetrigger records headSHA as the head we are (about to) trigger a Devin
// review for, returning true only if it was not already the recorded head. The
// compare-and-set runs under devinMu so concurrent callers (and the once-per-head
// cost guard) cannot both claim the same head. It is set BEFORE the network call
// so a failed/rate-limited trigger is not retried every poll.
func (s *CIStep) claimRetrigger(headSHA string) bool {
	s.devinMu.Lock()
	defer s.devinMu.Unlock()
	if s.lastRetriggeredSHA == headSHA {
		return false
	}
	s.lastRetriggeredSHA = headSHA
	return true
}

func (s *CIStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider := scm.DetectProvider(sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown && sctx.Run.PRURL != nil {
		provider = scm.DetectProvider(*sctx.Run.PRURL)
	}
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		sctx.Log(fmt.Sprintf("skipping CI: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	if err := host.Available(ctx); err != nil {
		sctx.Log(fmt.Sprintf("skipping CI: %v", err))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	// reviewLoop is the post-PR review loop (Devin). It is inert unless both the
	// config flag and the host's review capability are present; per-run disable
	// and cross-owner applicability skips disable only Devin while preserving CI
	// monitoring.
	loop := reviewLoopForRun(sctx, host)
	loopActive := loop.active
	if loop.reason != "" {
		sctx.Log(loop.reason)
	}

	// Get PR URL from run record
	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = *sctx.Run.PRURL
	}
	if prURL == "" {
		// Try to refresh from DB in case PR step set it
		run, _ := sctx.DB.GetRun(sctx.Run.ID)
		if run != nil && run.PRURL != nil {
			prURL = *run.PRURL
			sctx.Run.PRURL = run.PRURL
		}
	}
	if prURL == "" {
		sctx.Log("no PR URL found, skipping CI")
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return nil, fmt.Errorf("extract PR number: %w", err)
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}

	// CITimeout semantics: <0 (or "unlimited" in config) means never
	// self-terminate; 0 means the value was never configured, so fall back
	// to the default; >0 is an explicit finite idle timeout.
	timeout := sctx.Config.CITimeout
	unlimited := timeout < 0
	if timeout == 0 {
		timeout = config.DefaultCITimeout
	}

	if unlimited {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (no timeout, until merged or closed)...", prNumber))
	} else {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (timeout: %s)...", prNumber, timeout))
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	baseBranchTip := s.baseBranchTip
	if baseBranchTip == nil {
		baseBranchTip = func(ctx context.Context) (string, bool) {
			return resolveDefaultBranchTip(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
		}
	}
	started := now()
	// timeoutAnchor is the point the idle timeout is measured from. It re-arms
	// to now() whenever the base branch advances, while started stays fixed so
	// poll-interval and grace-period pacing are unaffected by re-arming.
	timeoutAnchor := started
	lastBaseTip := ""
	manualFixAttempted := false
	mergeabilityBlockedReason := ""
	timeoutFailingChecks := []string{}
	timeoutMergeConflict := false
	timeoutAutoResolve := ciPRClosedAutoResolver(sctx, host, pr)
	lastMonitorLog := ""
	timeoutOutcome := func() (*pipeline.StepOutcome, error) {
		sctx.Log("CI timeout reached")
		if len(timeoutFailingChecks) > 0 || timeoutMergeConflict {
			outcome := ciFailureOutcome(timeoutFailingChecks, timeoutMergeConflict, "CI timed out with known failures still present")
			outcome.ApprovalAutoResolve = timeoutAutoResolve
			return outcome, nil
		}
		if mergeabilityBlockedReason != "" {
			outcome := ciMergeabilityOutcome("mergeability check timed out", mergeabilityBlockedReason)
			outcome.ApprovalAutoResolve = timeoutAutoResolve
			return outcome, nil
		}
		outcome := ciMonitoringTimeoutOutcome()
		outcome.ApprovalAutoResolve = timeoutAutoResolve
		return outcome, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !unlimited && now().Sub(timeoutAnchor) >= timeout {
			return timeoutOutcome()
		}

		// Re-arm the timeout whenever the base branch advances.
		if !unlimited {
			resolveWindow := defaultBaseBranchTipResolveWindow
			if remaining := timeout - now().Sub(timeoutAnchor); remaining <= 0 {
				return timeoutOutcome()
			} else if remaining < resolveWindow {
				resolveWindow = remaining
			}
			tipCtx, cancel := context.WithTimeout(ctx, resolveWindow)
			tip, resolved := baseBranchTip(tipCtx)
			cancel()
			if resolved && tip != "" {
				if lastBaseTip == "" {
					lastBaseTip = tip
				} else if tip != lastBaseTip {
					sctx.Log(fmt.Sprintf("base branch advanced (%s..%s), re-arming CI monitor timeout", shortSHA(lastBaseTip), shortSHA(tip)))
					timeoutAnchor = now()
					lastBaseTip = tip
				}
			}
		}

		elapsed := now().Sub(started)
		if !unlimited && now().Sub(timeoutAnchor) >= timeout {
			return timeoutOutcome()
		}

		// Check PR state (merged/closed -> exit)
		prStateKnown := true
		state, err := host.GetPRState(ctx, pr)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check PR state: %v", err))
			prStateKnown = false
		} else if state == scm.PRStateMerged {
			sctx.Log("PR has been merged!")
			return &pipeline.StepOutcome{}, nil
		} else if state == scm.PRStateClosed {
			sctx.Log("PR has been closed")
			return &pipeline.StepOutcome{}, nil
		}

		// Check mergeable state if the provider supports it
		mergeConflict := false
		mergeabilityKnown := true
		if host.Capabilities().MergeableState {
			mergeState, mergeErr := host.GetMergeableState(ctx, pr)
			if mergeErr != nil {
				sctx.Log(fmt.Sprintf("warning: could not check mergeable state: %v", mergeErr))
				mergeabilityBlockedReason = ""
				mergeabilityKnown = false
			} else {
				mergeConflict = mergeState.Conflict()
				mergeabilityKnown = mergeState.Resolved()
				if !mergeabilityKnown {
					sctx.Log(fmt.Sprintf("mergeable state still pending: %s", mergeState))
					mergeabilityBlockedReason = fmt.Sprintf("PR mergeability remained unresolved before timeout: %s", mergeState)
				} else {
					mergeabilityBlockedReason = ""
					timeoutMergeConflict = mergeConflict
				}
			}
		}

		// Check CI status - wait for all checks to complete before fixing
		ciFixLimit := sctx.Config.AutoFix.CI
		checks, err := host.GetChecks(ctx, pr)
		if err != nil {
			lastMonitorLog = ""
			sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
		} else {
			pending := hasPendingChecks(checks)
			failing := failingCheckNames(checks)
			sort.Strings(failing)
			hasFailures := len(failing) > 0

			// Post-PR review loop: read the bot's verdict for the current head.
			// Only consults the host when the loop is active, so behavior is
			// unchanged otherwise. A "not green" verdict is folded into hasIssues
			// so the existing fix machinery (no-mistakes is the fixer) runs.
			devinDecision := devinDecisionDisabled
			var devinFindings []scm.ReviewComment
			var devinPrints []string
			if loopActive {
				devinDecision, devinFindings = s.evalDevinReview(sctx, host, pr)
				devinPrints = devinFindingFingerprints(devinFindings)
				// When Devin has not posted a verdict for the current head yet
				// (NONE/PENDING), explicitly (re-)trigger a review via the Devin API:
				// its auto-review is rate-limited / pausable, so this kicks a paused
				// initial review. Once-per-head guarded + best-effort (never alters
				// control flow); inert unless cfg.Retrigger and a key resolves.
				if devinDecision == devinDecisionPending {
					s.maybeRetriggerDevin(sctx, pr, sctx.Run.HeadSHA)
				}
			}
			devinNotGreen := devinDecision == devinDecisionNotGreen

			hasIssues := hasFailures || mergeConflict || devinNotGreen
			timeoutFailingChecks = append(timeoutFailingChecks[:0], failing...)

			// If a failing check completed after our last fix push, CI has
			// already re-run since we pushed (possibly too fast to observe
			// as pending between polls). Treat this as a new iteration so
			// the retry path can fire rather than looping on "fix already
			// attempted" until timeout.
			if failingCheckCompletedAfter(checks, s.lastFixedCompletedAt) {
				s.lastFixedChecks = ""
				s.lastFixedCompletedAt = nil
			}

			if hasIssues && pending {
				lastMonitorLog = ""
				if pendingCheckMatchesLastFixed(checks, s.lastFixedChecks) {
					s.lastFixedChecks = ""
					s.lastFixedCompletedAt = nil
				}
				sctx.Log("issues detected but checks still pending, waiting for all checks to complete...")
			} else if hasIssues {
				lastMonitorLog = ""
				// All checks done, issues present - fix or report. The CI-check
				// identity and the review-loop identity are kept as separate
				// anti-thrash dimensions (see ciFixAlreadyAttempted) so a push
				// that flips the review verdict to pending does not reset the CI
				// anti-thrash and re-run the fixer against still-stale checks.
				ciKey := encodeLastFixedChecks(failing, mergeConflict)
				devinKey := encodeDevinFixKey(nil, false, sctx.Run.HeadSHA, devinPrints)
				fixCompletedAt := failingCheckCompletionTimes(checks)
				issueDesc := strings.Join(failing, ", ")
				if mergeConflict {
					if issueDesc != "" {
						issueDesc += " + merge conflict"
					} else {
						issueDesc = "merge conflict"
					}
				}
				if loopActive && devinNotGreen && !hasFailures && !mergeConflict {
					// Checks are clean but the review bot requested changes:
					// run a bounded review-loop fix round. Anti-thrash keys on
					// (headSHA, finding fingerprints) so a pushed fix waits for
					// the bot to re-review the new commit before re-evaluating.
					// A user/agent `fix` response after the loop parked forces one
					// more round past the bounded guard; gate it to once per
					// execution (manualFixAttempted) so it cannot thrash the loop.
					forcedDevinFix := sctx.Fixing && !manualFixAttempted
					if forcedDevinFix {
						manualFixAttempted = true
					}
					if outcome := s.handleDevinFixRound(sctx, host, pr, devinFindings, devinKey, fixCompletedAt, forcedDevinFix); outcome != nil {
						return outcome, nil
					}
				} else if sctx.Fixing && !manualFixAttempted {
					manualFixAttempted = true
					sctx.Log(fmt.Sprintf("issues detected: %s - manual fix requested...", issueDesc))
					previousHeadSHA := sctx.Run.HeadSHA
					pushed, err := s.autoFixCI(sctx, host, pr, failing, mergeConflict, devinFindings)
					if err != nil {
						sctx.Log(fmt.Sprintf("warning: CI manual fix failed: %v", err))
					} else if pushed || sctx.Run.HeadSHA != previousHeadSHA {
						s.recordCIFix(ciKey, devinKey, devinNotGreen, fixCompletedAt)
					} else {
						sctx.Log("CI fix produced no changes, returning for manual intervention...")
						return ciFailureOutcome(failing, mergeConflict, "CI fix produced no changes - failures require manual intervention"), nil
					}
				} else if sctx.Fixing && s.ciFixAlreadyAttempted(ciKey, devinKey, devinNotGreen) {
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else if ciFixLimit <= 0 {
					sctx.Log(fmt.Sprintf("issues detected: %s - auto-fix disabled, waiting for manual intervention...", issueDesc))
					return ciFailureOutcome(failing, mergeConflict, "CI failures require manual intervention"), nil
				} else if s.ciFixAttempts >= ciFixLimit {
					sctx.Log(fmt.Sprintf("issues detected: %s - max auto-fix attempts (%d) reached, waiting for manual intervention...", issueDesc, ciFixLimit))
					return ciFailureOutcome(failing, mergeConflict, "CI failures still present after auto-fix attempts"), nil
				} else if s.ciFixAlreadyAttempted(ciKey, devinKey, devinNotGreen) {
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else {
					s.ciFixAttempts++
					sctx.Log(fmt.Sprintf("issues detected: %s - auto-fixing (attempt %d/%d)...", issueDesc, s.ciFixAttempts, ciFixLimit))
					previousHeadSHA := sctx.Run.HeadSHA
					pushed, err := s.autoFixCI(sctx, host, pr, failing, mergeConflict, devinFindings)
					if err != nil {
						sctx.Log(fmt.Sprintf("warning: CI auto-fix failed: %v", err))
					} else if pushed || sctx.Run.HeadSHA != previousHeadSHA {
						s.recordCIFix(ciKey, devinKey, devinNotGreen, fixCompletedAt)
					} else {
						// No changes produced - don't set lastFixedChecks so next
						// poll treats this as a new failure and retries if attempts remain.
						sctx.Log("CI fix produced no changes, will retry if attempts remain...")
					}
				}
			} else {
				s.lastFixedChecks = ""
				s.lastFixedCompletedAt = nil
				passedMsg := ciChecksPassedMsg
				if len(checks) == 0 {
					passedMsg = ciNoChecksPassedMsg
				}
				switch {
				case !prStateKnown || !mergeabilityKnown:
					lastMonitorLog = ""
				case pending:
					// Checks are (re-)running with no failures yet. Surface this
					// so a PR that passed checks and starts re-running clears the
					// previous passed-checks signal instead of looking stale.
					lastMonitorLog = logCIMonitorStatus(sctx, ciChecksRunningMsg, lastMonitorLog)
				case loopActive && devinDecision == devinDecisionPending:
					// Checks are clean but the review bot has not posted a verdict
					// for the current head yet: not ready to merge. Surfacing this
					// (instead of "checks passed") keeps the agent from declaring
					// the run done before the review lands.
					reviewMsg := cimonitor.WaitingOnReviewMsg
					if s.devinRounds() > 0 {
						reviewMsg = cimonitor.ReReviewingMsg
					}
					lastMonitorLog = logCIMonitorStatus(sctx, reviewMsg, lastMonitorLog)
				case len(checks) == 0 && elapsed < s.gracePeriod():
					// CI checks may not be registered yet, keep polling.
					lastMonitorLog = ""
					sctx.Log("no CI checks reported yet, waiting for checks to register...")
				case loopActive && devinDecision == devinDecisionFailOpen:
					// The bot stayed silent past the grace window and fail-open is
					// configured: fall back to checks-only green, but loudly.
					if passedMsg != lastMonitorLog {
						sctx.Log("WARNING: Devin posted no review within the grace window; proceeding on checks-only green (review_loop.fail_open=true)")
					}
					lastMonitorLog = logCIMonitorStatus(sctx, passedMsg, lastMonitorLog)
				case len(checks) == 0:
					lastMonitorLog = logCIMonitorStatus(sctx, ciNoChecksPassedMsg, lastMonitorLog)
				default:
					lastMonitorLog = logCIMonitorStatus(sctx, ciChecksPassedMsg, lastMonitorLog)
				}
			}
		}

		// Sleep for poll interval
		interval := s.pollIntervalOverride
		if interval == 0 {
			interval = pollInterval(now().Sub(started))
		}
		if !unlimited {
			remaining := timeout - now().Sub(timeoutAnchor)
			if remaining < interval {
				interval = remaining
			}
		}
		waitForNextPoll := s.waitForNextPoll
		if waitForNextPoll == nil {
			waitForNextPoll = func(ctx context.Context, interval time.Duration) error {
				select {
				case <-time.After(interval):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if err := waitForNextPoll(ctx, interval); err != nil {
			return nil, err
		}
	}
}

func logCIMonitorStatus(sctx *pipeline.StepContext, message, previous string) string {
	if message != previous {
		sctx.Log(message)
	}
	return message
}
