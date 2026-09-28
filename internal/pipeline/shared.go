package pipeline

import "sync"

// HousekeepingLintResult is the lint assessment produced by the combined
// document+lint housekeeping pass: the document step performs both duties in
// one agent invocation and hands the lint half to the lint step so it does
// not pay a second cold agent pass.
type HousekeepingLintResult struct {
	// FindingsJSON holds the lint-category findings (possibly an empty set)
	// in the same JSON shape the lint step produces itself.
	FindingsJSON string
	// Summary is the housekeeping pass's one-line lint summary.
	Summary string
}

// RunShared carries in-memory run-scoped results one step hands to a later
// step in the same run. It lives on the executor for the run's lifetime and
// is never persisted: on any process boundary the consuming step simply
// falls back to doing its own work.
type RunShared struct {
	mu               sync.Mutex
	prepared         bool
	housekeepingLint *HousekeepingLintResult

	// tierMu guards only the size tier and review risk, never mu: mu is held
	// across commands.prepare, and a tier read must not wait on it.
	tierMu     sync.Mutex
	sizeTier   *SizeTier
	reviewRisk string
}

// SetSizeTier records the run's size classification. The first call wins:
// the Review step classifies the submitted diff once and fix rounds do not
// reclassify.
func (s *RunShared) SetSizeTier(tier SizeTier) {
	if s == nil {
		return
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	if s.sizeTier == nil {
		s.sizeTier = &tier
	}
}

// SizeTier returns the run's size classification; ok is false when this
// executor never classified the run (tiers off, Review skipped, or a daemon
// restart), and consumers then keep their untiered behavior.
func (s *RunShared) SizeTier() (SizeTier, bool) {
	if s == nil {
		return SizeTier{}, false
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	if s.sizeTier == nil {
		return SizeTier{}, false
	}
	return *s.sizeTier, true
}

// SetReviewRisk records the risk_level of the latest review round.
func (s *RunShared) SetReviewRisk(level string) {
	if s == nil {
		return
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	s.reviewRisk = level
}

// ReviewRisk returns the latest review round's risk_level, or "" when none
// was recorded by this executor.
func (s *RunShared) ReviewRisk() string {
	if s == nil {
		return ""
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	return s.reviewRisk
}

// EnsurePrepared runs prepare once successfully for this executor lifetime.
// A failed attempt is not cached, so a caller may retry after the underlying
// problem is corrected.
func (s *RunShared) EnsurePrepared(prepare func() error) error {
	if s == nil {
		return prepare()
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	if s.prepared {
		return nil
	}
	if err := prepare(); err != nil {
		return err
	}
	s.prepared = true
	return nil
}

// SetHousekeepingLint records the combined pass's lint assessment for the
// lint step. It replaces any previous assessment (a document fix round
// re-runs the combined pass and re-stashes a fresh result).
func (s *RunShared) SetHousekeepingLint(result HousekeepingLintResult) {
	if s == nil {
		return
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	s.housekeepingLint = &result
}

// ClearHousekeepingLint discards a previous combined-pass lint assessment
// before a document pass starts, so a later lint step never consumes stale
// findings.
func (s *RunShared) ClearHousekeepingLint() {
	if s == nil {
		return
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	s.housekeepingLint = nil
}

// TakeHousekeepingLint returns and consumes the combined pass's lint
// assessment. The second call returns false so a lint fix round re-assesses
// with its own agent pass instead of trusting a stale result.
func (s *RunShared) TakeHousekeepingLint() (HousekeepingLintResult, bool) {
	if s == nil {
		return HousekeepingLintResult{}, false
	}
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	if s.housekeepingLint == nil {
		return HousekeepingLintResult{}, false
	}
	result := *s.housekeepingLint
	s.housekeepingLint = nil
	return result, true
}
