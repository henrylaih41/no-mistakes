package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestRepoSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https", "https://github.com/test/repo", "test/repo"},
		{"https with .git suffix", "https://github.com/test/repo.git", "test/repo"},
		{"pr url", "https://github.com/test/repo/pull/42", "test/repo"},
		{"ssh scp form", "git@github.com:test/repo.git", "test/repo"},
		{"ssh scp form no suffix", "git@github.com:test/repo", "test/repo"},
		{"ssh url form", "ssh://git@github.com/test/repo.git", "test/repo"},
		{"https with port", "https://github.com:8443/test/repo", "test/repo"},
		{"already a slug", "test/repo", "test/repo"},
		{"trailing slash", "https://github.com/test/repo/", "test/repo"},
		{"empty", "", ""},
		{"host only", "https://github.com/", ""},
		{"owner only", "https://github.com/onlyowner", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoSlug(tc.in); got != tc.want {
				t.Fatalf("RepoSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGetChecksPassesRepoFlag(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt": {
			stdout: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]` + "\n",
		},
	}), nil, "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("checks = %+v, want single build check", checks)
	}
}

func TestGetPRStatePassesRepoFlag(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json state --jq .state": {
			stdout: "MERGED\n",
		},
	}), nil, "test/repo")

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateMerged)
	}
}

func TestGetChecksFallsBackToStateWhenBucketMissing(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket,completedAt": {
			stdout: `[{"name":"build","state":"FAILURE","bucket":""},{"name":"tests","state":"PENDING","bucket":""}]` + "\n",
		},
	}), nil, "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(checks))
	}
	if checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("checks[0] = %+v, want failing build check", checks[0])
	}
	if checks[1].Name != "tests" || checks[1].Bucket != scm.CheckBucketPending {
		t.Fatalf("checks[1] = %+v, want pending tests check", checks[1])
	}
}

func TestGetChecksParsesCompletedAt(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket,completedAt": {
			stdout: `[{"name":"build","state":"FAILURE","bucket":"fail","completedAt":"2026-04-24T04:15:00Z"},{"name":"tests","state":"SUCCESS","bucket":"pass","completedAt":"not-a-time"}]` + "\n",
		},
	}), nil, "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(checks))
	}

	wantCompletedAt := time.Date(2026, 4, 24, 4, 15, 0, 0, time.UTC)
	if !checks[0].CompletedAt.Equal(wantCompletedAt) {
		t.Fatalf("checks[0].CompletedAt = %v, want %v", checks[0].CompletedAt, wantCompletedAt)
	}
	if !checks[1].CompletedAt.IsZero() {
		t.Fatalf("checks[1].CompletedAt = %v, want zero time for invalid timestamp", checks[1].CompletedAt)
	}
}

func TestFetchFailedCheckLogsSelectsMatchingRunForHeadSHA(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh run list --branch feature --commit abc123 --status failure --limit 20 --json databaseId,headSha,name,displayTitle,workflowName": {
			stdout: `[{"databaseId":101,"headSha":"abc123","name":"CI","displayTitle":"feature","workflowName":"CI"},{"databaseId":102,"headSha":"abc123","name":"Lint","displayTitle":"lint","workflowName":"Lint"}]` + "\n",
		},
		"gh run view 101 --json jobs": {
			stdout: `{"jobs":[{"name":"unit","conclusion":"failure"}]}` + "\n",
		},
		"gh run view 102 --json jobs": {
			stdout: `{"jobs":[{"name":"lint","conclusion":"failure"}]}` + "\n",
		},
		"gh run view 102 --log-failed": {
			stdout: "lint failed\n",
		},
	}), nil, "")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "123"}, "feature", "abc123", []string{"lint"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "lint failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want %q", logs, "lint failed")
	}
}

func TestFindPRFiltersByBaseBranch(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature/refactor --base release/1.0 --state open --json number,url": {
			stdout: `[{"number":42,"url":"https://github.example.com/org/repo/pull/42"}]` + "\n",
		},
	}), nil, "")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "release/1.0")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want PR")
	}
	if pr.Number != "42" {
		t.Fatalf("FindPR() number = %q, want %q", pr.Number, "42")
	}
	if pr.URL != "https://github.example.com/org/repo/pull/42" {
		t.Fatalf("FindPR() URL = %q, want matching base PR", pr.URL)
	}
}

func TestFindPRForkUsesBareHeadAndFiltersOwner(t *testing.T) {
	t.Parallel()

	branch := "feature/refactor"
	host := NewWithFork(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head fork-owner:" + branch + " --base main --repo parent/repo --state open --json number,url,headRefName,headRepositoryOwner": {
			stderr: `invalid argument: "--head" does not support "<owner>:<branch>"` + "\n",
			code:   1,
		},
		"gh pr list --head " + branch + " --base main --repo parent/repo --state open --json number,url,headRefName,headRepositoryOwner": {
			stdout: `[` +
				`{"number":40,"url":"https://github.com/parent/repo/pull/40","headRefName":"feature/refactor","headRepositoryOwner":{"login":"other-owner"}},` +
				`{"number":42,"url":"https://github.com/parent/repo/pull/42","headRefName":"feature/refactor","headRepositoryOwner":{"login":"fork-owner"}}` +
				`]` + "\n",
		},
	}), nil, "parent/repo", "fork-owner/repo")

	pr, err := host.FindPR(context.Background(), branch, "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want fork PR")
	}
	if pr.Number != "42" {
		t.Fatalf("FindPR() number = %q, want 42", pr.Number)
	}
	if pr.URL != "https://github.com/parent/repo/pull/42" {
		t.Fatalf("FindPR() URL = %q, want fork-owned parent PR", pr.URL)
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature/refactor --base main --state open --json number,url": {
			stderr: "api unavailable\n",
			code:   1,
		},
	}), nil, "")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "main")
	if err == nil {
		t.Fatal("FindPR() error = nil, want CLI error")
	}
	if !strings.Contains(err.Error(), "gh pr list") {
		t.Fatalf("FindPR() error = %v, want gh pr list context", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() PR = %+v, want nil", pr)
	}
}

// Canned `gh api` JSON modeled on a real Devin PR: the bot posts a COMMENTED
// review (never CHANGES_REQUESTED) with inline findings whose bodies carry the
// 🔴 (bug/high) and 🚩 (analysis/medium) severity markers, plus a top-level
// summary comment that carries no SHA.
const (
	headSHA = "abc123def"
	oldSHA  = "0000oldsha"
	botUser = "devin-ai-integration[bot]"
)

// inlineCommentsJSON has, in order: a 🔴 finding on headSHA, a 🚩 finding on
// headSHA, a stale 🔴 finding on an OLD sha (must be scoped out), and a human's
// inline comment on headSHA (must be filtered out by login).
const inlineCommentsJSON = `[
  {"path":"internal/batch/download.go","line":42,"original_line":40,"body":"🔴 **Batch download crashes on empty manifest**","commit_id":"abc123def","original_commit_id":"abc123def","html_url":"https://github.com/test/repo/pull/7#discussion_r1","user":{"login":"devin-ai-integration[bot]"}},
  {"path":"internal/batch/path.go","line":17,"original_line":17,"body":"🚩 **Batch path swallows the second error**","commit_id":"abc123def","original_commit_id":"abc123def","html_url":"https://github.com/test/repo/pull/7#discussion_r2","user":{"login":"devin-ai-integration[bot]"}},
  {"path":"internal/old/stale.go","line":3,"original_line":3,"body":"🔴 **Stale finding from a previous commit**","commit_id":"0000oldsha","original_commit_id":"0000oldsha","html_url":"https://github.com/test/repo/pull/7#discussion_r0","user":{"login":"devin-ai-integration[bot]"}},
  {"path":"internal/human/note.go","line":9,"original_line":9,"body":"high severity concern from a human","commit_id":"abc123def","original_commit_id":"abc123def","html_url":"https://github.com/test/repo/pull/7#discussion_rh","user":{"login":"some-human"}}
]` + "\n"

// issueCommentsJSON has a Devin top-level summary (kept, no SHA, no Path) and a
// human's reply (filtered out).
const issueCommentsJSON = `[
  {"body":"Devin reviewed the latest changes; see inline comments.","html_url":"https://github.com/test/repo/pull/7#issuecomment-1","user":{"login":"devin-ai-integration[bot]"}},
  {"body":"thanks!","html_url":"https://github.com/test/repo/pull/7#issuecomment-2","user":{"login":"some-human"}}
]` + "\n"

func TestGetBotFindingsFiltersBotParsesSeverityAndScopesToHead(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/comments --paginate":  {stdout: inlineCommentsJSON},
		"gh api repos/test/repo/issues/7/comments --paginate": {stdout: issueCommentsJSON},
	}), nil, "test/repo")

	findings, err := host.GetBotFindings(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetBotFindings() error = %v", err)
	}
	if len(findings) != 3 {
		t.Fatalf("len(findings) = %d, want 3 (two head-scoped inline + one top-level; stale-sha and human dropped): %+v", len(findings), findings)
	}

	// findings[0]: 🔴 inline -> high, file-scoped to headSHA.
	if findings[0].Severity != "high" {
		t.Errorf("findings[0].Severity = %q, want high", findings[0].Severity)
	}
	if findings[0].Path != "internal/batch/download.go" {
		t.Errorf("findings[0].Path = %q, want internal/batch/download.go", findings[0].Path)
	}
	if findings[0].Line != 42 {
		t.Errorf("findings[0].Line = %d, want 42", findings[0].Line)
	}
	if findings[0].CommitID != headSHA {
		t.Errorf("findings[0].CommitID = %q, want %q", findings[0].CommitID, headSHA)
	}
	if findings[0].URL == "" {
		t.Errorf("findings[0].URL is empty, want the discussion URL")
	}

	// findings[1]: 🚩 inline -> medium.
	if findings[1].Severity != "medium" {
		t.Errorf("findings[1].Severity = %q, want medium", findings[1].Severity)
	}

	// findings[2]: top-level Devin summary -> no Path/Line/CommitID, default medium.
	if findings[2].Path != "" || findings[2].Line != 0 || findings[2].CommitID != "" {
		t.Errorf("findings[2] = %+v, want top-level (empty Path/Line/CommitID)", findings[2])
	}
	if findings[2].Severity != "medium" {
		t.Errorf("findings[2].Severity = %q, want medium (default for unmarked body)", findings[2].Severity)
	}
}

func TestGetReviewVerdictChangesRequested(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]"}}]` + "\n",
		},
		"gh api repos/test/repo/pulls/7/comments --paginate":  {stdout: inlineCommentsJSON},
		"gh api repos/test/repo/issues/7/comments --paginate": {stdout: issueCommentsJSON},
	}), nil, "test/repo")

	verdict, findings, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("GetReviewVerdict() = %q, want %q (severe inline findings on headSHA)", verdict, scm.VerdictChangesRequested)
	}
	// The verdict path returns the findings it read so the caller need not refetch.
	if len(findings) != 3 {
		t.Fatalf("GetReviewVerdict() findings = %d, want 3 (returned alongside the verdict)", len(findings))
	}
}

func TestGetReviewVerdictHonorsChangesRequestedState(t *testing.T) {
	t.Parallel()

	// A native CHANGES_REQUESTED review on the head with NO inline finding that
	// parses as severe must still be not-green: the explicit request-changes state
	// is authoritative on its own.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"CHANGES_REQUESTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]"}}]` + "\n",
		},
		"gh api repos/test/repo/pulls/7/comments --paginate": {
			stdout: `[{"path":"a.go","line":1,"body":"please follow up here","commit_id":"abc123def","original_commit_id":"abc123def","html_url":"u","user":{"login":"devin-ai-integration[bot]"}}]` + "\n",
		},
		"gh api repos/test/repo/issues/7/comments --paginate": {stdout: `[]` + "\n"},
	}), nil, "test/repo")

	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("GetReviewVerdict() = %q, want %q (native CHANGES_REQUESTED state honored)", verdict, scm.VerdictChangesRequested)
	}
}

func TestGetReviewVerdictApproved(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]"}}]` + "\n",
		},
		// Bot reviewed headSHA but left only a top-level summary (no file-scoped
		// findings), and a stale finding from a prior commit that scopes out.
		"gh api repos/test/repo/pulls/7/comments --paginate": {
			stdout: `[{"path":"internal/old/stale.go","line":3,"body":"🔴 **Stale finding from a previous commit**","commit_id":"0000oldsha","original_commit_id":"0000oldsha","html_url":"u","user":{"login":"devin-ai-integration[bot]"}}]` + "\n",
		},
		"gh api repos/test/repo/issues/7/comments --paginate": {stdout: issueCommentsJSON},
	}), nil, "test/repo")

	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictApproved {
		t.Fatalf("GetReviewVerdict() = %q, want %q (reviewed headSHA, no severe head-scoped findings)", verdict, scm.VerdictApproved)
	}
}

func TestGetReviewVerdictPendingWhenHeadNotYetReviewed(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		// Bot has reviewed before, but only an older commit - not headSHA.
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"0000oldsha","user":{"login":"devin-ai-integration[bot]"}}]` + "\n",
		},
		// The bot only reviewed an older commit, so its inline comments scope out
		// of headSHA: no head-scoped findings remain.
		"gh api repos/test/repo/pulls/7/comments --paginate":  {stdout: `[]` + "\n"},
		"gh api repos/test/repo/issues/7/comments --paginate": {stdout: `[]` + "\n"},
	}), nil, "test/repo")

	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictPending {
		t.Fatalf("GetReviewVerdict() = %q, want %q (bot reviewed an older sha, not headSHA)", verdict, scm.VerdictPending)
	}
}

func TestGetReviewVerdictNoneWhenBotNeverReviewed(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		// Only a human review exists; the bot has never reviewed.
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"APPROVED","commit_id":"abc123def","user":{"login":"some-human"}}]` + "\n",
		},
	}), nil, "test/repo")

	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictNone {
		t.Fatalf("GetReviewVerdict() = %q, want %q (bot never reviewed)", verdict, scm.VerdictNone)
	}
}

func TestGetReviewVerdictPaginatesPastFirstPage(t *testing.T) {
	t.Parallel()

	// `gh api --paginate` merges every page into one JSON array. Model a response
	// whose first 30 entries are benign (low) and whose 31st - which only exists
	// because page 2 was fetched - is a severe finding tied to headSHA. If the
	// read stopped at page 1 this severe finding would be dropped and the gate
	// would wrongly approve.
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"path":"p%d.go","line":%d,"body":"nit: minor style","commit_id":"abc123def","original_commit_id":"abc123def","html_url":"u%d","user":{"login":"devin-ai-integration[bot]"}}`, i, i+1, i)
	}
	b.WriteString(`,{"path":"deep/page2.go","line":99,"body":"🔴 **Severe bug only visible on page 2**","commit_id":"abc123def","original_commit_id":"abc123def","html_url":"u-page2","user":{"login":"devin-ai-integration[bot]"}}`)
	b.WriteString("]\n")

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]"}}]` + "\n",
		},
		"gh api repos/test/repo/pulls/7/comments --paginate":  {stdout: b.String()},
		"gh api repos/test/repo/issues/7/comments --paginate": {stdout: `[]` + "\n"},
	}), nil, "test/repo")

	findings, err := host.GetBotFindings(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetBotFindings() error = %v", err)
	}
	if len(findings) != 31 {
		t.Fatalf("GetBotFindings() = %d findings, want 31 (all pages merged)", len(findings))
	}
	sawPage2 := false
	for _, f := range findings {
		if f.Path == "deep/page2.go" {
			sawPage2 = true
			if f.Severity != "high" {
				t.Errorf("page-2 finding severity = %q, want high", f.Severity)
			}
		}
	}
	if !sawPage2 {
		t.Fatal("page-2 finding was dropped: pagination not applied to PR review comments")
	}

	// The severe page-2 finding must flip the verdict to changes-requested.
	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("GetReviewVerdict() = %q, want CHANGES_REQUESTED (severe finding beyond page 1)", verdict)
	}
}

func TestSeverityFromBody(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body string
		want string
	}{
		{"🔴 **Batch download crashes**", "high"},
		{"🚩 **Batch path swallows error**", "medium"},
		{"This is a clear bug in the loop", "high"},
		{"high severity issue", "high"},
		{"medium severity issue", "medium"},
		{"low severity issue", "low"},
		{"nit: rename this", "low"},
		{"some unmarked observation", "medium"},
		// Word-boundary matching: "low" as a substring of these words must not
		// downgrade an otherwise-unmarked (default medium) finding to low.
		{"please follow up on this", "medium"},
		{"we should allow this pattern", "medium"},
		{"this sits just below the limit", "medium"},
		{"the data should flow through here", "medium"},
	}
	for _, tc := range cases {
		if got := severityFromBody(tc.body); got != tc.want {
			t.Errorf("severityFromBody(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

type githubTestResponse struct {
	stdout string
	stderr string
	code   int
}

func githubTestCmdFactory(responses map[string]githubTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
			response = githubTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"GITHUB_TEST_HELPER=1",
			"GITHUB_TEST_STDOUT="+response.stdout,
			"GITHUB_TEST_STDERR="+response.stderr,
			fmt.Sprintf("GITHUB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestGitHubHelperProcess(t *testing.T) {
	if os.Getenv("GITHUB_TEST_HELPER") != "1" {
		return
	}

	if _, err := fmt.Fprint(os.Stdout, os.Getenv("GITHUB_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("GITHUB_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("GITHUB_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
