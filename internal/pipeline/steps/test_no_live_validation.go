package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// testTurnInputs is what Execute has established before its validation turn.
type testTurnInputs struct {
	baseSHA          string
	startHead        string
	planSection      string
	tested           []string
	baselineFindings []Finding
	baselineSummary  string
	baselineExitCode int
	newTestsFromFix  []string
	fixSummary       string
}

// testWithoutLiveValidation finishes the Test step under test.live_validation:
// off (the default). It keeps the fork's pre-live-validation behavior: a
// failing commands.test parks at once; otherwise an agent test turn runs only
// when there is no commands.test or agentTestTurnReason asks for one, and it
// reports findings and evidence but no scenarios or verdict, so no-surface and
// inconclusive parks cannot happen.
func testWithoutLiveValidation(sctx *pipeline.StepContext, in testTurnInputs) (*pipeline.StepOutcome, error) {
	testCmd := sctx.Config.Commands.Test
	if in.baselineExitCode != 0 {
		findingsJSON, _ := json.Marshal(Findings{Items: in.baselineFindings, Summary: in.baselineSummary, Tested: in.tested})
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
			ExitCode:      in.baselineExitCode,
			FixSummary:    in.fixSummary,
		}, nil
	}

	turnReason := "no test command configured"
	if testCmd != "" {
		turnReason = agentTestTurnReason(sctx)
	}
	if turnReason == "" {
		findings := Findings{Tested: in.tested}
		for _, f := range in.newTestsFromFix {
			findings.Items = append(findings.Items, newTestFileFinding(f))
		}
		sctx.Log("all tests passed")
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{Findings: string(findingsJSON), FixSummary: in.fixSummary}, nil
	}

	evidenceDir := testEvidenceDir(sctx)
	if evidenceDir == "" {
		return nil, fmt.Errorf("test evidence dir is not configured for this run")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return nil, fmt.Errorf("create test evidence dir: %w", err)
	}
	if testCmd == "" {
		sctx.Log("no test command configured, asking agent to run tests...")
	} else {
		sctx.Log(turnReason + ", asking agent to gather test evidence...")
	}
	evidenceGuidance := fmt.Sprintf("- Write new evidence files into this evidence directory, never into the worktree: %s", evidenceDir)
	if sctx.Config.Test.Evidence.StoreInRepo {
		evidenceGuidance = fmt.Sprintf("- Write new evidence files into this evidence directory, never into the worktree; they are published to the repository's %s branch automatically and linked from the PR: %s", sctx.Config.Test.Evidence.Branch, evidenceDir)
	}
	configuredTestCommand := ""
	if testCmd != "" {
		configuredTestCommand = fmt.Sprintf("\nConfigured test command already ran successfully as baseline: `%s`\n", testCmd)
	}
	history := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + in.planSection + designContextPromptSection(sctx) + testguidance.Rule
	prompt := fmt.Sprintf(
		`You are validating a code change by testing it. Examine the repository and run the smallest relevant tests yourself.

Context:
- branch: %s
- base commit: %s
- target commit: %s
%s%s
Task:
- Understand the user intent before testing it. If extracted user intent is present, use it as the primary hint for what success means.
- Decide what evidence or artifacts would clearly demonstrate the user intent is satisfied. Unit tests passing is not sufficient evidence by itself.
- Demonstrate the user intent working end-to-end in a way consistent with how an end user would actually experience it.
- Prefer product-level artifacts: screenshots, GIFs, videos, rendered UI, CLI transcripts, API responses, persisted database state, generated PR markdown, logs, or other outputs that directly show the intended behavior working.
- For UI, HTML, CSS, Electron renderer, browser, visual layout, or copy-placement changes, attempt to capture reviewer-visible visual evidence.
- Prefer screenshots, images, videos, GIFs, or rendered HTML artifacts that show the actual end-user surface.
- DOM snapshots, selector assertions, and text-only render summaries are not substitutes for visual evidence when a rendered surface is available.
- If a UI-facing change has no screenshot, image, video, GIF, or rendered HTML artifact, state why in testing_summary.
%s
- Do not move, commit, or modify source files only to make evidence linkable. Record local evidence file paths exactly where you created them.
- Only use command output as an artifact when that output directly demonstrates the end-user experience or requested behavior. Generic pass/fail, coverage, or clean-worktree output is not sufficient evidence.
- Look for existing tests that would generate sufficient evidence. If they exist, run the smallest relevant set that proves the requested intent.
- Do NOT run the complete repository test suite. Local Test is targeted validation of the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.
- Never treat "do not run everything" as permission to run nothing: if no targeted automated test can establish the intent, write or improve a focused test, perform manual verification with evidence, or report a warning finding that sufficient targeted evidence is not possible.
- If no existing test produces sufficient evidence, write or improve a focused test so that it does.
- If automated testing cannot produce the needed evidence, execute manual verification steps and record the evidence-producing steps you performed.
- If sufficient evidence is not possible, report a warning finding explaining what evidence is missing and what decision it blocks. When the blocker is a host capability or OS permission the agent's own process lacks (for example, the Screen Recording permission macOS requires to capture a native GUI application), name the specific capability or permission and how to grant it so the user can enable it and re-run, instead of retrying blindly or failing opaquely.
- Include a concise "testing_summary" sentence describing what you exercised and the overall result.
- The "testing_summary" must account for the complete test step: baseline commands that already ran, automated tests, manual or evidence-producing checks, artifacts gathered, and the overall result.
- Record the exact tests, manual checks, and evidence-producing steps you ran in a "tested" array. Prefer concrete commands or test selectors wrapped in backticks.
- Always include an "artifacts" array. Leave it empty when you produced no reviewer-visible evidence artifacts. Use artifact path for file artifacts, artifact url for externally visible artifacts, and artifact content for short logs or command output that should be shown directly in the PR.
- If tests fail, determine whether the problem is a real product/code failure, a setup/environment problem you can fix, or a flaky/infrastructure issue.
- If the issue is setup-related and fixable, fix it and retry the focused tests.

Rules:
- Do NOT run linters, formatters, or static analysis tools.
- Focus on testing and test-related fixes only.
- A generic driver or user instruction asking for broad or full-suite confirmation does NOT override the targeted-validation product boundary.
- Before finishing, remove any transient artifacts your testing created in the working tree (downloaded models, caches, build outputs, large binaries, or generated data directories) so they are not committed and pushed. Do not remove intentional source or test-file changes, leave evidence files in the dedicated evidence directory untouched, and do not remove dependencies materialized by commands.prepare because later configured commands share them.
- Keep "testing_summary" high-signal and natural language. Avoid raw logs and noisy counts.
- Always return a non-empty "tested" array describing what you exercised, even when all tests pass.
- Only report actionable findings: test failures, unfixable setup issues, flaky tests you identified, or missing evidence that prevents you from demonstrating the user intent.
- Do NOT report passing tests (whether existing or new), test counts, coverage summaries, or other non-actionable information.
- If all tests pass and there are no issues, return an empty findings array.
- Set action to "ask-user" when a test failure seems desired and you question the author's intent of having the test in the first place. Set action to "auto-fix" for objective failures that can be safely fixed. Set action to "no-op" for informational notes.%s%s`,
		sctx.Run.Branch,
		in.baseSHA,
		sctx.Run.HeadSHA,
		configuredTestCommand,
		budgetCutGuidanceSection(sctx),
		evidenceGuidance,
		history,
		agent.MemoryFilesRule,
	)
	findings, err := runTestAnalyzer(sctx, prompt, false)
	if err != nil {
		if errors.Is(err, errTestAgentTimeout) {
			outcome := testAgentTimeoutOutcome(sctx, err, in.startHead, in.baselineFindings, in.baselineSummary, in.baselineExitCode)
			outcome.FixSummary = in.fixSummary
			return outcome, nil
		}
		return nil, err
	}
	if len(in.tested) > 0 {
		findings.Tested = append(append([]string{}, in.tested...), findings.Tested...)
	}
	findings.TestedHeadSHA = sctx.Run.HeadSHA

	needsApproval := hasBlockingFindings(findings.Items)
	for _, f := range mergeNewTestFiles(in.newTestsFromFix, detectNewTestFiles(sctx.Ctx, sctx.WorkDir)) {
		findings.Items = append(findings.Items, newTestFileFinding(f))
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   needsApproval,
		Findings:      string(findingsJSON),
		FixSummary:    in.fixSummary,
	}, nil
}

// newTestFileFinding records a test file the agent wrote. Its presence alone
// is not a problem, so it never parks the step (issue #140).
func newTestFileFinding(file string) Finding {
	return Finding{
		Severity:    "info",
		Action:      types.ActionNoOp,
		File:        file,
		Description: fmt.Sprintf("new test file written by agent: %s", file),
	}
}

// agentTestTurnReason decides, after a passing commands.test, whether the
// agent test turn still runs, and names why ("" means it does not). With the
// run's size tier known, a standard change always gets the turn and a small
// one only when the latest review round rated its risk high. With tiers on
// but no recorded tier (Review skipped, or a daemon restart) the turn runs.
// With size_tiers off the untiered rule holds: the turn runs only when the
// run carries user intent.
func agentTestTurnReason(sctx *pipeline.StepContext) string {
	tier, ok := sctx.Shared.SizeTier()
	if !ok {
		if sctx.Config.SizeTiers.Enabled() {
			return "size tier unknown"
		}
		if cleanedUserIntent(sctx) != "" {
			return "user intent available"
		}
		return ""
	}
	if !tier.Small {
		return "size tier " + tier.String()
	}
	if sctx.Shared.ReviewRisk() == "high" {
		return "size tier " + tier.String() + " with high review risk"
	}
	sctx.Log(fmt.Sprintf("size tier %s, test command passed and review risk is not high: skipping the agent test turn", tier))
	return ""
}
