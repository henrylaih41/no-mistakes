package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewerReport is one reviewer's parsed findings after its IDs have been
// namespaced (review-<name>-<slot>-N) and every item's Source stamped with
// the reviewer name, so the merged union stays attributable to its origin.
type reviewerReport struct {
	Name     string
	Findings types.Findings
}

// runReviewPanel fans the review prompt out across every reviewer concurrently
// and merges their reports into a single attributed union. opts carries the
// shared review prompt/schema/CWD. With a single reviewer, streaming is kept
// because only one fan-out goroutine can call LogChunk while the main goroutine
// waits; with multiple reviewers, OnChunk is forced to nil because
// StepContext.Log/LogChunk mutate shared state and are not goroutine-safe. All
// multi-reviewer logging and merging happens serially on this goroutine after
// FanOut returns. It enforces the fail policy: a reviewer error fails the step
// unless review.fail_open is set. Fail-closed panels cancel siblings on the
// first error because their results will be discarded; fail-open panels wait
// for every reviewer because every successful report is still useful.
//
// Reviewers are READ-ONLY by contract: they inspect the diff and return
// findings, and never write to the worktree. The shared CWD is therefore
// intentional and safe, and we deliberately do not isolate or clean up a
// per-reviewer worktree. A reviewer that writes files is a misconfiguration,
// not a scenario this code defends against - so shared-worktree concurrency is
// not a data-safety issue here and should not be flagged as one.
func runReviewPanel(sctx *pipeline.StepContext, reviewers []agent.Agent, opts agent.RunOpts) (Findings, error) {
	if len(reviewers) == 1 {
		opts.OnChunk = sctx.LogChunk
	} else {
		opts.OnChunk = nil
	}

	failOpen := sctx.Config.Review.FailOpen
	var results []agent.FanOutResult
	if failOpen {
		results = agent.FanOut(sctx.Ctx, reviewers, opts, sctx.Config.Review.MaxParallel)
	} else {
		results = agent.FanOutCancelOnError(sctx.Ctx, reviewers, opts, sctx.Config.Review.MaxParallel)
	}

	reports, err := processReviewerResults(results, failOpen, sctx.Log, sctx.LogFile)
	if err != nil {
		return Findings{}, err
	}

	// Per-reviewer user-visible summary, emitted serially from the main
	// goroutine now that every reviewer has finished.
	for _, r := range reports {
		risk := r.Findings.RiskLevel
		if risk == "" {
			risk = "none"
		}
		sctx.Log(fmt.Sprintf("[reviewer %s] %d finding(s), risk=%s", r.Name, len(r.Findings.Items), risk))
	}

	return combineReviewerFindings(reports), nil
}

// processReviewerResults turns FanOut results into attributed reviewer reports,
// in reviewer (input) order. Each successful reviewer's findings are parsed with
// the same parser the single-reviewer path uses, ID-namespaced to
// review-<name>-<slot>-N where slot is the reviewer's stable input position
// (collision-free across reviewers, including two same-family reviewers - the
// per-slot index disambiguates them and does not shift when review.fail_open
// drops an earlier reviewer; any model-supplied id is discarded so a reviewer
// cannot smuggle in a colliding id), Source-stamped with the reviewer name, and
// its raw report written to the file-only audit log.
//
// Fail policy: when failOpen is false (the default) the first reviewer error
// fails the step with an error naming that reviewer family. When failOpen is
// true a failed reviewer is dropped with a loud, user-visible warning and the
// step continues only if at least one reviewer succeeded. log is the
// user-visible callback; logFile is the file-only audit callback. Both run on
// the caller's goroutine.
func processReviewerResults(results []agent.FanOutResult, failOpen bool, log, logFile func(string)) ([]reviewerReport, error) {
	if !failOpen {
		if failed := firstReviewerError(results); failed != nil {
			return nil, fmt.Errorf("review panel: reviewer %q failed: %w", failed.name, failed.err)
		}
	}

	reports := make([]reviewerReport, 0, len(results))
	var dropped []string
	for idx, res := range results {
		name := res.Agent.Name()
		if res.Err != nil {
			dropped = append(dropped, name)
			log(fmt.Sprintf("WARNING: reviewer %q failed and was DROPPED (review.fail_open=true): %v", name, res.Err))
			if logFile != nil {
				logFile(fmt.Sprintf("[reviewer %s] ERROR: %v", name, res.Err))
			}
			continue
		}
		parsed := parseReviewFindings(res.Result, log)
		prefix := fmt.Sprintf("review-%s-%d", name, idx+1)
		for i := range parsed.Items {
			parsed.Items[i].ID = ""
		}
		parsed = types.NormalizeFindings(parsed, prefix)
		for i := range parsed.Items {
			parsed.Items[i].Source = name
		}
		reports = append(reports, reviewerReport{Name: name, Findings: parsed})
		if logFile != nil {
			if raw, mErr := json.Marshal(parsed); mErr == nil {
				logFile(fmt.Sprintf("[reviewer %s] report: %s", name, string(raw)))
			}
		}
	}
	if len(reports) == 0 {
		return nil, fmt.Errorf("review panel: all reviewers failed (%s)", strings.Join(dropped, ", "))
	}
	return reports, nil
}

type reviewerError struct {
	name string
	err  error
}

func firstReviewerError(results []agent.FanOutResult) *reviewerError {
	var first *reviewerError
	for _, res := range results {
		if res.Err == nil {
			continue
		}
		current := &reviewerError{name: res.Agent.Name(), err: res.Err}
		if first == nil {
			first = current
		}
		// FanOutCancelOnError may make an earlier slot report context.Canceled
		// after a later reviewer genuinely failed. Prefer the original backend
		// error so the fail-closed message names the reviewer that doomed the
		// step.
		if !errors.Is(res.Err, context.Canceled) {
			return current
		}
	}
	return first
}

// combineReviewerFindings merges reviewer reports into a plain attributed union.
// Items are concatenated in reviewer (input) order, each keeping the
// review-<name>-<slot>-N id and Source set by processReviewerResults - there is NO
// fingerprint dedup, agreement-collapse, or severity-escalation. The scalar
// fields are reconciled: RiskLevel is the maximum (low < medium < high) across
// reports, while RiskRationale, Summary, and TestingSummary become per-reviewer
// labeled concatenations ("[codex] ...; [claude] ...") so the fix agent and
// human can see who said what. Tested and Artifacts evidence is concatenated in
// reviewer order so multi-reviewer mode preserves the same fields the
// single-reviewer path round-trips.
func combineReviewerFindings(reports []reviewerReport) types.Findings {
	var merged types.Findings
	rationales := make([]string, 0, len(reports))
	summaries := make([]string, 0, len(reports))
	testingSummaries := make([]string, 0, len(reports))
	for _, r := range reports {
		merged.Items = append(merged.Items, r.Findings.Items...)
		merged.Tested = append(merged.Tested, r.Findings.Tested...)
		merged.Artifacts = append(merged.Artifacts, r.Findings.Artifacts...)
		if types.RiskRank(r.Findings.RiskLevel) > types.RiskRank(merged.RiskLevel) {
			merged.RiskLevel = r.Findings.RiskLevel
		}
		if s := strings.TrimSpace(r.Findings.RiskRationale); s != "" {
			rationales = append(rationales, fmt.Sprintf("[%s] %s", r.Name, s))
		}
		if s := strings.TrimSpace(r.Findings.Summary); s != "" {
			summaries = append(summaries, fmt.Sprintf("[%s] %s", r.Name, s))
		}
		if s := strings.TrimSpace(r.Findings.TestingSummary); s != "" {
			testingSummaries = append(testingSummaries, fmt.Sprintf("[%s] %s", r.Name, s))
		}
	}
	merged.RiskRationale = strings.Join(rationales, "; ")
	merged.Summary = strings.Join(summaries, "; ")
	merged.TestingSummary = strings.Join(testingSummaries, "; ")
	return merged
}
