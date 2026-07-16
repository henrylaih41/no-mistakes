package github

import (
	"context"
	"fmt"
	"io"
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

func TestHostPrefixedSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// github.com inputs keep the plain owner/name format.
		{"github.com https", "https://github.com/test/repo", "test/repo"},
		{"github.com https with .git suffix", "https://github.com/test/repo.git", "test/repo"},
		{"github.com pr url", "https://github.com/test/repo/pull/42", "test/repo"},
		{"github.com ssh scp form", "git@github.com:test/repo.git", "test/repo"},
		{"github.com ssh url form", "ssh://git@github.com/test/repo.git", "test/repo"},
		{"github.com https with port", "https://github.com:8443/test/repo", "test/repo"},
		{"github.com mixed case host", "https://GitHub.com/test/repo.git", "test/repo"},
		{"github.com trailing slash", "https://github.com/test/repo/", "test/repo"},

		// GitHub Enterprise Server inputs get the host prefix gh requires.
		{"ghe https", "https://bbgithub.dev.bloomberg.com/org/repo", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe https with .git suffix", "https://bbgithub.dev.bloomberg.com/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe ssh scp form", "git@bbgithub.dev.bloomberg.com:org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe ssh url form", "ssh://git@bbgithub.dev.bloomberg.com/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe pr url", "https://bbgithub.dev.bloomberg.com/org/repo/pull/42", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe https with port", "https://bbgithub.dev.bloomberg.com:8443/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe trailing slash", "https://bbgithub.dev.bloomberg.com/org/repo/", "bbgithub.dev.bloomberg.com/org/repo"},

		// Empty/malformed inputs return "" so the --repo flag is omitted.
		{"empty", "", ""},
		{"host only ghe", "https://bbgithub.dev.bloomberg.com/", ""},
		{"owner only ghe", "https://bbgithub.dev.bloomberg.com/onlyowner", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostPrefixedSlug(tc.in); got != tc.want {
				t.Fatalf("HostPrefixedSlug(%q) = %q, want %q", tc.in, got, tc.want)
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
	}), nil, "", "test/repo")

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
	}), nil, "", "test/repo")

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateMerged)
	}
}

func TestCreatePRStreamsBodyThroughStdin(t *testing.T) {
	t.Parallel()

	const body = "## What Changed\n\n- keep generated pull request bodies postable"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr create --head feature/body-cap --base main --repo test/repo --title fix: cap body --body-file -": {
			stdout:    "https://github.com/test/repo/pull/42\n",
			wantStdin: body,
		},
	}), nil, "", "test/repo")

	pr, err := host.CreatePR(context.Background(), "feature/body-cap", "main", scm.PRContent{
		Title: "fix: cap body",
		Body:  body,
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "42" {
		t.Fatalf("CreatePR() PR = %+v, want #42", pr)
	}
}

func TestUpdatePRStreamsBodyThroughStdin(t *testing.T) {
	t.Parallel()

	const body = "## What Changed\n\n- update existing pull request bodies without long argv"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr edit 42 --repo test/repo --title fix: cap body --body-file -": {
			wantStdin: body,
		},
	}), nil, "", "test/repo")

	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	updated, err := host.UpdatePR(context.Background(), pr, scm.PRContent{
		Title: "fix: cap body",
		Body:  body,
	})
	if err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if updated != pr {
		t.Fatalf("UpdatePR() = %+v, want original PR", updated)
	}
}

func TestGetChecksFallsBackToStateWhenBucketMissing(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket,completedAt": {
			stdout: `[{"name":"build","state":"FAILURE","bucket":""},{"name":"tests","state":"PENDING","bucket":""}]` + "\n",
		},
	}), nil, "", "")

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
	}), nil, "", "")

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
	}), nil, "", "")

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
	}), nil, "", "")

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
	}), nil, "", "parent/repo", "fork-owner/repo")

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
	}), nil, "", "")

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

func TestAvailableScopesAuthToConfiguredHost(t *testing.T) {
	t.Parallel()

	// With a known host, the auth check must be scoped via --hostname so a
	// stale credential on some other configured gh host (e.g. github.com vs
	// a GHE instance) cannot make this repo look unauthenticated. The
	// unscoped form is treated as a failure here to prove the scoped form
	// is the one actually invoked.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status --hostname ghe.example.com": {},
		"gh auth status": {stderr: "github.com: token invalid\n", code: 1},
	}), func() bool { return true }, "ghe.example.com", "")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil (scoped auth should pass)", err)
	}
}

func TestAvailableFallsBackToUnscopedAuthWhenHostUnknown(t *testing.T) {
	t.Parallel()

	// No host -> behave as before: a bare `gh auth status`.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status": {},
	}), func() bool { return true }, "", "")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

// Canned `gh api graphql` data modeled on a real Devin PR. The read layer now
// reads review THREADS (not flat REST comments) so it can honor each thread's
// isResolved/isOutdated state: GitHub re-anchors a bot's old comments onto the
// head, so a REST read reports already-addressed comments as live and the
// verdict never clears. Only live (unresolved AND not-outdated) bot threads are
// findings.
const (
	headSHA = "abc123def"
	// botUser is the REST-form bot login (with "[bot]" suffix), used for REST
	// review mocks and as the configured botLogin in tests that exercise the
	// default config form.
	botUser = "devin-ai-integration[bot]"
	// botSlug is the REAL GraphQL-form bot login (bare app slug, no "[bot]"
	// suffix). GraphQL's reviewThreads API returns this for a Bot actor. Tests
	// that mock GraphQL thread authors use this so they exercise the actual
	// API shape rather than the REST form the buggy code assumed.
	botSlug = "devin-ai-integration"
)

// graphqlThreadsKey is the cmd-factory key for the `gh api graphql` reviewThreads
// read GetBotFindings issues. It is derived from the same args builder the
// production code uses, so the canned response is keyed byte-for-byte as the call
// site emits it (the query string carries spaces, so hand-writing the key is
// brittle).
func graphqlThreadsKey(repo string, prNumber int, cursor ...string) string {
	h := &Host{repo: repo}
	c := ""
	if len(cursor) > 0 {
		c = cursor[0]
	}
	return strings.TrimSpace("gh " + strings.Join(h.reviewThreadsArgs(prNumber, c), " "))
}

// reviewThreadsResponse wraps thread nodes in the graphql envelope the production
// parser expects. It models a SINGLE page (hasNextPage:false); use
// reviewThreadsPage to model a paginated response.
func reviewThreadsResponse(nodes ...string) githubTestResponse {
	return reviewThreadsPage(false, "", nodes...)
}

// reviewThreadsPage renders one page of the reviewThreads envelope, including the
// pageInfo{hasNextPage endCursor} the production cursor-pagination loop consumes.
func reviewThreadsPage(hasNextPage bool, endCursor string, nodes ...string) githubTestResponse {
	return githubTestResponse{
		stdout: fmt.Sprintf(
			`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[`,
			hasNextPage, endCursor,
		) + strings.Join(nodes, ",") + `]}}}}}` + "\n",
	}
}

// threadID renders one reviewThreads node: its resolution/outdated flags plus a
// single anchoring comment (author/databaseId/path/line/body). The author's
// __typename is inferred from the login: a "[bot]"-suffixed login OR the known
// bot slug (real GraphQL returns the slug without the suffix) is a Bot; anything
// else is a User. This mirrors what real GraphQL returns — a Bot actor's
// __typename is "Bot" and its login is the bare app slug.
func threadID(databaseID int64, resolved, outdated bool, author, path string, line int, body string) string {
	typename := "User"
	if strings.HasSuffix(strings.ToLower(author), "[bot]") || strings.EqualFold(author, botSlug) {
		typename = "Bot"
	}
	return fmt.Sprintf(
		`{"isResolved":%t,"isOutdated":%t,"comments":{"nodes":[{"author":{"login":%q,"__typename":%q},"databaseId":%d,"path":%q,"line":%d,"originalLine":%d,"body":%q,"url":"https://github.com/test/repo/pull/7#discussion"}]}}`,
		resolved, outdated, author, typename, databaseID, path, line, line, body,
	)
}

// thread is threadID with a zero databaseId, for tests that don't exercise the id.
func thread(resolved, outdated bool, author, path string, line int, body string) string {
	return threadID(0, resolved, outdated, author, path, line, body)
}

// threadWithCommit is threadID with an explicit originalCommit { oid } field on
// the first comment, modeling the real GraphQL API shape. The oid is the head
// SHA the thread was originally posted on. GetBotFindings must filter threads
// whose originalCommit.oid does not match the current headSHA so stale threads
// from old heads (already fixed but not marked outdated) don't drive redundant
// fix rounds or get redundant "Addressed in" replies.
func threadWithCommit(databaseID int64, resolved, outdated bool, author, path string, line int, body, commitOID string) string {
	typename := "User"
	if strings.HasSuffix(strings.ToLower(author), "[bot]") || strings.EqualFold(author, botSlug) {
		typename = "Bot"
	}
	return fmt.Sprintf(
		`{"isResolved":%t,"isOutdated":%t,"comments":{"nodes":[{"author":{"login":%q,"__typename":%q},"databaseId":%d,"path":%q,"line":%d,"originalLine":%d,"body":%q,"url":"https://github.com/test/repo/pull/7#discussion","originalCommit":{"oid":%q}}]}}`,
		resolved, outdated, author, typename, databaseID, path, line, line, body, commitOID,
	)
}

// liveThreads is the MIX used across the read-layer tests: two LIVE bot findings
// (🔴 high + 🚩 medium), one OUTDATED bot finding (the anchored code changed =
// addressed), one RESOLVED bot finding (someone closed it), and one LIVE
// human-authored thread (fails the bot-login filter). Only the two live bot
// threads are findings.
//
// The bot threads use the REAL GraphQL login (botSlug, no "[bot]" suffix) so
// the fixtures exercise the actual API shape: prior to the login-normalization
// fix the mock used the REST form ("devin-ai-integration[bot]"), which real
// GraphQL never returns, so the mock agreed with the buggy comparison and hid
// the false-green bug.
func liveThreads() []string {
	return []string{
		thread(false, false, botSlug, "internal/batch/download.go", 42, "🔴 **Batch download crashes on empty manifest**"),
		thread(false, false, botSlug, "internal/batch/path.go", 17, "🚩 **Batch path swallows the second error**"),
		thread(false, true, botSlug, "internal/old/outdated.go", 3, "🔴 **Outdated: the code this anchored to changed**"),
		thread(true, false, botSlug, "internal/old/resolved.go", 5, "🔴 **Resolved: someone marked this done**"),
		thread(false, false, "some-human", "internal/human/note.go", 9, "🔴 high severity concern from a human"),
	}
}

func TestGetBotFindingsReturnsOnlyLiveBotThreads(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(liveThreads()...),
	}), nil, "", "test/repo")

	findings, err := host.GetBotFindings(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetBotFindings() error = %v", err)
	}
	// Only the two LIVE bot threads survive: the outdated and resolved bot threads
	// are addressed, and the human thread fails the first-comment-author filter.
	if len(findings) != 2 {
		t.Fatalf("len(findings) = %d, want 2 (live bot threads only; outdated/resolved/human dropped): %+v", len(findings), findings)
	}

	// Every returned finding must be file-scoped (a thread with a path anchor).
	for i, f := range findings {
		if f.Path == "" {
			t.Errorf("findings[%d] = %+v, want a file-scoped finding", i, f)
		}
	}

	// findings[0]: 🔴 -> high, file-scoped, with the thread comment URL preserved.
	if findings[0].Severity != "high" {
		t.Errorf("findings[0].Severity = %q, want high", findings[0].Severity)
	}
	if findings[0].Path != "internal/batch/download.go" {
		t.Errorf("findings[0].Path = %q, want internal/batch/download.go", findings[0].Path)
	}
	if findings[0].Line != 42 {
		t.Errorf("findings[0].Line = %d, want 42", findings[0].Line)
	}
	if findings[0].URL == "" {
		t.Errorf("findings[0].URL is empty, want the discussion URL")
	}

	// findings[1]: 🚩 -> medium.
	if findings[1].Severity != "medium" {
		t.Errorf("findings[1].Severity = %q, want medium", findings[1].Severity)
	}

	// No outdated/resolved finding may leak in: none of the addressed paths appear.
	for _, f := range findings {
		switch f.Path {
		case "internal/old/outdated.go", "internal/old/resolved.go", "internal/human/note.go":
			t.Errorf("addressed/non-bot thread leaked as a finding: %+v", f)
		}
	}
}

func TestGetReviewVerdictChangesRequested(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(liveThreads()...)}), nil, "", "test/repo")

	verdict, findings, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("GetReviewVerdict() = %q, want %q (a live severe finding remains)", verdict, scm.VerdictChangesRequested)
	}
	// The verdict path returns the findings it read so the caller need not refetch:
	// two LIVE findings (the outdated/resolved bot threads and the human thread are
	// excluded).
	if len(findings) != 2 {
		t.Fatalf("GetReviewVerdict() findings = %d, want 2 (live findings returned alongside the verdict)", len(findings))
	}
}

func TestDecodePaginatedArray(t *testing.T) {
	t.Parallel()

	// Empty / whitespace body decodes to nil without error.
	for _, in := range []string{"", "  \n"} {
		got, err := decodePaginatedArray[ghReview]([]byte(in))
		if err != nil {
			t.Fatalf("decodePaginatedArray(%q) error = %v", in, err)
		}
		if got != nil {
			t.Fatalf("decodePaginatedArray(%q) = %v, want nil", in, got)
		}
	}

	// A single page (one JSON array) decodes as one value.
	single := `[{"state":"APPROVED","commit_id":"a"}]`
	got, err := decodePaginatedArray[ghReview]([]byte(single))
	if err != nil {
		t.Fatalf("single-page decode error = %v", err)
	}
	if len(got) != 1 || got[0].State != "APPROVED" {
		t.Fatalf("single-page decode = %+v, want one APPROVED", got)
	}

	// Multiple pages: `gh api --paginate` (no --slurp) emits each page as its own
	// top-level array document concatenated back-to-back. They must all be read.
	multi := `[{"state":"COMMENTED","commit_id":"a"}]` + "\n" +
		`[{"state":"CHANGES_REQUESTED","commit_id":"b"},{"state":"APPROVED","commit_id":"c"}]` + "\n"
	got, err = decodePaginatedArray[ghReview]([]byte(multi))
	if err != nil {
		t.Fatalf("multi-page decode error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("multi-page decode = %d reviews, want 3 (pages must be flattened)", len(got))
	}
	if got[1].State != "CHANGES_REQUESTED" || got[1].CommitID != "b" {
		t.Fatalf("multi-page decode lost page-2 content: %+v", got)
	}
}

// TestGetReviewVerdictReadsAllReviewPages guards the false-green path: when the
// head's CHANGES_REQUESTED review lands on a second pagination page, the verdict
// read must still surface it instead of failing to parse the multi-document
// `gh api --paginate` output and degrading to VerdictNone.
func TestGetReviewVerdictReadsAllReviewPages(t *testing.T) {
	t.Parallel()

	page1 := `[{"state":"COMMENTED","commit_id":"older","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n"
	page2 := `[{"state":"CHANGES_REQUESTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n"

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: page1 + page2,
		},
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(),
	}), nil, "", "test/repo")

	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("GetReviewVerdict() = %q, want %q (head review on page 2 must be read)", verdict, scm.VerdictChangesRequested)
	}
}

func TestGetReviewVerdictHonorsChangesRequestedState(t *testing.T) {
	t.Parallel()

	// A native CHANGES_REQUESTED review on the head with NO inline finding that
	// parses as severe must still be not-green: the explicit request-changes state
	// is authoritative on its own.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"CHANGES_REQUESTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		// One LIVE thread whose body parses as a non-severe nit, so only the native
		// CHANGES_REQUESTED state can be driving the verdict.
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(
			thread(false, false, botUser, "a.go", 1, "nit: please follow up here"),
		)}), nil, "", "test/repo")

	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("GetReviewVerdict() = %q, want %q (native CHANGES_REQUESTED state honored)", verdict, scm.VerdictChangesRequested)
	}
}

// TestGetReviewVerdictUsesMostRecentHeadReview verifies the verdict follows the
// MOST RECENT bot review on the head (by submitted_at), not the OR of every
// state in the head's review history: a bot that requested changes and then
// re-reviewed the SAME SHA as APPROVED must clear (and vice versa).
func TestGetReviewVerdictUsesMostRecentHeadReview(t *testing.T) {
	t.Parallel()

	// Earlier CHANGES_REQUESTED, later APPROVED on the same head, no severe inline
	// findings: the most recent review approves, so the verdict clears.
	approvedLast := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"CHANGES_REQUESTED","commit_id":"abc123def","submitted_at":"2026-01-01T00:00:00Z","user":{"login":"devin-ai-integration[bot]","type":"Bot"}},{"state":"APPROVED","commit_id":"abc123def","submitted_at":"2026-01-01T01:00:00Z","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(),
	}), nil, "", "test/repo")

	verdict, _, err := approvedLast.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictApproved {
		t.Fatalf("GetReviewVerdict() = %q, want %q (most recent head review approved)", verdict, scm.VerdictApproved)
	}

	// Reverse order (timestamps deliberately out of array order): a later
	// CHANGES_REQUESTED after an earlier APPROVED stays not-green.
	changesLast := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"CHANGES_REQUESTED","commit_id":"abc123def","submitted_at":"2026-01-01T02:00:00Z","user":{"login":"devin-ai-integration[bot]","type":"Bot"}},{"state":"APPROVED","commit_id":"abc123def","submitted_at":"2026-01-01T01:00:00Z","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(),
	}), nil, "", "test/repo")

	verdict2, _, err := changesLast.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict2 != scm.VerdictChangesRequested {
		t.Fatalf("GetReviewVerdict() = %q, want %q (most recent head review requested changes)", verdict2, scm.VerdictChangesRequested)
	}
}

// TestGetReviewVerdictConvergesWhenSevereThreadsAddressed is the convergence
// case: the bot reviewed the head and its only severe threads are now outdated
// (code changed) or resolved. With no LIVE severe finding and no native
// CHANGES_REQUESTED state, the verdict must clear to APPROVED so the post-PR
// review loop can finally converge — Devin re-anchors old comments onto the head,
// so a REST read would still count these as live and the loop would never clear.
func TestGetReviewVerdictConvergesWhenSevereThreadsAddressed(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		// Two severe bot threads, but both are addressed: one outdated, one resolved.
		// No live severe finding remains.
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(
			thread(false, true, botUser, "internal/old/outdated.go", 3, "🔴 **Outdated severe finding**"),
			thread(true, false, botUser, "internal/old/resolved.go", 5, "🔴 **Resolved severe finding**"),
		)}), nil, "", "test/repo")

	verdict, findings, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictApproved {
		t.Fatalf("GetReviewVerdict() = %q, want %q (all severe threads outdated/resolved)", verdict, scm.VerdictApproved)
	}
	if len(findings) != 0 {
		t.Fatalf("GetReviewVerdict() findings = %d, want 0 (no live findings remain): %+v", len(findings), findings)
	}
}

func TestGetReviewVerdictPendingWhenHeadNotYetReviewed(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		// Bot has reviewed before, but only an older commit - not headSHA.
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"0000oldsha","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		// No live review threads remain on the head.
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse()}), nil, "", "test/repo")

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
	}), nil, "", "test/repo")

	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictNone {
		t.Fatalf("GetReviewVerdict() = %q, want %q (bot never reviewed)", verdict, scm.VerdictNone)
	}
}

// TestGetReviewVerdictEmptyHeadSHAIsNotYetReviewed verifies the fail-safe for an
// empty (or whitespace-only) head SHA: without a head to scope to, the verdict
// must be VerdictNone with no findings instead of treating the empty SHA as a
// wildcard that matches every review/comment in the PR's history. It must also
// short-circuit before any `gh` round-trip, so the cmd factory is given no
// responses (any call would fail the command).
func TestGetReviewVerdictEmptyHeadSHAIsNotYetReviewed(t *testing.T) {
	t.Parallel()

	for _, head := range []string{"", "   "} {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{}), nil, "", "test/repo")
		verdict, findings, err := host.GetReviewVerdict(context.Background(), 7, head, botUser)
		if err != nil {
			t.Fatalf("GetReviewVerdict(head=%q) error = %v", head, err)
		}
		if verdict != scm.VerdictNone {
			t.Fatalf("GetReviewVerdict(head=%q) = %q, want %q (can't scope to a head)", head, verdict, scm.VerdictNone)
		}
		if len(findings) != 0 {
			t.Fatalf("GetReviewVerdict(head=%q) findings = %d, want 0", head, len(findings))
		}
	}
}

// TestGetBotFindingsEmptyHeadSHAIsFailSafe verifies the empty-head fail-safe:
// with no head SHA the read layer reports no findings and short-circuits before
// any gh round-trip (the cmd factory is given no responses, so any call would
// fail the command). This mirrors GetReviewVerdict's empty-head behavior.
func TestGetBotFindingsEmptyHeadSHAIsFailSafe(t *testing.T) {
	t.Parallel()

	for _, head := range []string{"", "   "} {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{}), nil, "", "test/repo")

		findings, err := host.GetBotFindings(context.Background(), 7, head, botUser)
		if err != nil {
			t.Fatalf("GetBotFindings(head=%q) error = %v", head, err)
		}
		if len(findings) != 0 {
			t.Fatalf("GetBotFindings(head=%q) = %d findings, want 0 (fail-safe, no gh call)", head, len(findings))
		}
	}
}

// TestGetBotFindingsCollectsAllLiveThreadsAndSevereFlipsVerdict models a busy PR:
// many live low/nit threads plus one live severe thread, interleaved with
// addressed (outdated/resolved) severe threads that must NOT count. The read
// layer must return every live thread and the verdict must be CHANGES_REQUESTED
// because a live severe finding remains.
func TestGetBotFindingsCollectsAllLiveThreadsAndSevereFlipsVerdict(t *testing.T) {
	t.Parallel()

	var nodes []string
	for i := 0; i < 30; i++ {
		nodes = append(nodes, thread(false, false, botUser, fmt.Sprintf("p%d.go", i), i+1, "nit: minor style"))
	}
	// Addressed severe threads (must be excluded) and one live severe thread.
	nodes = append(nodes,
		thread(false, true, botUser, "addressed/outdated.go", 7, "🔴 **Outdated severe finding**"),
		thread(true, false, botUser, "addressed/resolved.go", 8, "🔴 **Resolved severe finding**"),
		thread(false, false, botUser, "deep/live.go", 99, "🔴 **Live severe bug**"),
	)

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(nodes...)}), nil, "", "test/repo")

	findings, err := host.GetBotFindings(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetBotFindings() error = %v", err)
	}
	// 30 live nits + 1 live severe = 31; the two addressed severe threads drop out.
	if len(findings) != 31 {
		t.Fatalf("GetBotFindings() = %d findings, want 31 (all live threads, addressed excluded)", len(findings))
	}
	sawLiveSevere := false
	for _, f := range findings {
		switch f.Path {
		case "deep/live.go":
			sawLiveSevere = true
			if f.Severity != "high" {
				t.Errorf("live severe finding severity = %q, want high", f.Severity)
			}
		case "addressed/outdated.go", "addressed/resolved.go":
			t.Errorf("addressed thread leaked as a finding: %+v", f)
		}
	}
	if !sawLiveSevere {
		t.Fatal("live severe finding was dropped")
	}

	// The live severe finding must flip the verdict to changes-requested.
	verdict, _, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("GetReviewVerdict() = %q, want CHANGES_REQUESTED (a live severe finding remains)", verdict)
	}
}

// TestGetReviewVerdictPaginatesReviewThreads guards the false-APPROVED risk: when
// the PR's review threads span more than one page, a live severe finding that
// lands on page 2 must still be seen. reviewThreads returns threads oldest-first
// and counts addressed/resolved threads toward the first:100 window, so without
// cursor pagination the newest severe finding could be truncated past page 1 and
// the verdict would wrongly read APPROVED.
func TestGetReviewVerdictPaginatesReviewThreads(t *testing.T) {
	t.Parallel()

	// Page 1 carries only non-severe / already-addressed threads, so on its own it
	// would read APPROVED. The cursor advances to page 2.
	page1 := []string{
		thread(false, false, botUser, "p1-nit.go", 1, "nit: minor style"),
		thread(false, true, botUser, "p1-outdated.go", 2, "🔴 **Outdated severe (addressed)**"),
	}
	// Page 2 carries the LIVE severe finding that must flip the verdict.
	page2 := []string{
		thread(false, false, botUser, "p2-live.go", 9, "🔴 **Live severe bug on page 2**"),
	}

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		graphqlThreadsKey("test/repo", 7):            reviewThreadsPage(true, "CURSOR1", page1...),
		graphqlThreadsKey("test/repo", 7, "CURSOR1"): reviewThreadsPage(false, "", page2...),
	}), nil, "", "test/repo")

	verdict, findings, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetReviewVerdict() error = %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("verdict = %q, want CHANGES_REQUESTED (the live severe finding on page 2 must be seen, not truncated)", verdict)
	}
	sawPage2 := false
	for _, f := range findings {
		if f.Path == "p2-live.go" {
			sawPage2 = true
		}
	}
	if !sawPage2 {
		t.Fatalf("page-2 live severe finding missing from accumulated findings: %+v", findings)
	}
}

// TestGetBotFindingsPopulatesCommentID asserts the graphql databaseId is surfaced
// as ReviewComment.ID, which the review loop needs to thread a reply on a finding.
func TestGetBotFindingsPopulatesCommentID(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(
			threadID(987654321, false, false, botUser, "a.go", 3, "🔴 **bug**"),
		)}), nil, "", "test/repo")

	findings, err := host.GetBotFindings(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetBotFindings() error = %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(findings))
	}
	if findings[0].ID != 987654321 {
		t.Fatalf("findings[0].ID = %d, want 987654321 (the comment databaseId)", findings[0].ID)
	}
}

// TestReplyToReviewComment asserts the POST is issued to the replies endpoint with
// the body passed as a raw -f field.
func TestReplyToReviewComment(t *testing.T) {
	t.Parallel()

	const body = "Addressed in deadbeef by no-mistakes."
	key := "gh api repos/test/repo/pulls/7/comments/4242/replies -f body=" + body
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		key: {stdout: `{"id":1}` + "\n"},
	}), nil, "", "test/repo")

	if err := host.ReplyToReviewComment(context.Background(), 7, 4242, body); err != nil {
		t.Fatalf("ReplyToReviewComment() error = %v", err)
	}
}

// TestReplyToReviewCommentSurfacesError asserts a failed gh call returns an error
// (so the caller can log it best-effort).
func TestReplyToReviewCommentSurfacesError(t *testing.T) {
	t.Parallel()

	// No canned response => the factory returns a non-zero exit for the call.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{}), nil, "", "test/repo")
	if err := host.ReplyToReviewComment(context.Background(), 7, 4242, "body"); err == nil {
		t.Fatal("expected an error when the gh api replies call fails")
	}
}

func TestSeverityFromBody(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body string
		want string
	}{
		// high requires the explicit 🔴 marker; 🚩 marks medium.
		{"🔴 **Batch download crashes**", "high"},
		{"🚩 **Batch path swallows error**", "medium"},
		{"low severity issue", "low"},
		{"nit: rename this", "low"},
		{"some unmarked observation", "medium"},
		// The keyword fallback never escalates to high: bare/negated/contextual
		// mentions of severe words must NOT register as high (they default to the
		// conservative medium, which still gates).
		{"This is a clear bug in the loop", "medium"},
		{"this is not a bug", "medium"},
		{"no critical issues here", "medium"},
		{"high severity issue", "medium"},
		{"medium severity issue", "medium"},
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

// TestGetBotFindingsRealGraphQLLogin is the Bug 1 regression test: with the
// REAL GraphQL login (bare slug "devin-ai-integration", no "[bot]" suffix) and
// __typename "Bot", GetBotFindings must return the bot's live findings. Before
// the login-normalization fix, the bare-slug login never matched the configured
// "devin-ai-integration[bot]", so every thread was skipped and 0 findings were
// returned — the false-green root cause.
func TestGetBotFindingsRealGraphQLLogin(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(
			thread(false, false, botSlug, "internal/batch/download.go", 42, "🔴 **crash on empty manifest**"),
		),
	}), nil, "", "test/repo")

	findings, err := host.GetBotFindings(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetBotFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1 (real GraphQL slug login must match [bot]-form config): %v", len(findings), findings)
	}
}

// TestGetReviewVerdictSlugFormConfig asserts a slug-form config login
// ("devin-ai-integration", no "[bot]") still matches a REST "[bot]"-form review
// author — the symmetric direction of the normalization. A maintainer who
// configures the bare slug must not get a false "never reviewed" verdict.
func TestGetReviewVerdictSlugFormConfig(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/pulls/7/reviews --paginate": {
			stdout: `[{"state":"COMMENTED","commit_id":"abc123def","user":{"login":"devin-ai-integration[bot]","type":"Bot"}}]` + "\n",
		},
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(
			thread(false, false, botSlug, "internal/batch/download.go", 42, "🔴 **crash**"),
		),
	}), nil, "", "test/repo")

	verdict, findings, err := host.GetReviewVerdict(context.Background(), 7, headSHA, botSlug)
	if err != nil {
		t.Fatalf("GetReviewVerdict: %v", err)
	}
	if verdict != scm.VerdictChangesRequested {
		t.Fatalf("verdict = %q, want %q (slug-form config must match REST [bot]-form review)", verdict, scm.VerdictChangesRequested)
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(findings))
	}
}

// TestGetBotFindingsRejectsHuman asserts a human-authored thread (even one whose
// body parses as severe) is never counted as a bot finding. The actorKind
// positive-bot check (not just login matching) enforces this: a human's
// __typename is "User", so the thread is skipped regardless of login.
func TestGetBotFindingsRejectsHuman(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(
			thread(false, false, "some-human", "internal/human/note.go", 9, "🔴 **human high severity**"),
		),
	}), nil, "", "test/repo")

	findings, err := host.GetBotFindings(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetBotFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("len(findings) = %d, want 0 (human thread must be rejected by actorKind)", len(findings))
	}
}

// TestNormalizeBotLogin covers the login normalization helper directly: both
// REST and GraphQL forms normalize to the same bare slug, and humans pass
// through unchanged.
func TestNormalizeBotLogin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"devin-ai-integration[bot]", "devin-ai-integration"},
		{"devin-ai-integration", "devin-ai-integration"},
		{"Devin-AI-Integration[bot]", "devin-ai-integration"},
		{"  devin-ai-integration[bot]  ", "devin-ai-integration"},
		{"some-human", "some-human"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeBotLogin(c.in); got != c.want {
			t.Errorf("normalizeBotLogin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBotLoginMatch covers the cross-form matching that the false-green bug
// broke: a GraphQL bare-slug author must match a REST "[bot]"-form config, and
// vice versa, while a human never matches the bot config.
func TestBotLoginMatch(t *testing.T) {
	t.Parallel()
	if !botLoginMatch("devin-ai-integration", "devin-ai-integration[bot]") {
		t.Error(`botLoginMatch("devin-ai-integration", "devin-ai-integration[bot]") = false, want true (GraphQL slug vs REST config)`)
	}
	if !botLoginMatch("devin-ai-integration[bot]", "devin-ai-integration") {
		t.Error(`botLoginMatch("devin-ai-integration[bot]", "devin-ai-integration") = false, want true (REST author vs slug config)`)
	}
	if !botLoginMatch("devin-ai-integration[bot]", "devin-ai-integration[bot]") {
		t.Error(`botLoginMatch("devin-ai-integration[bot]", "devin-ai-integration[bot]") = false, want true (REST vs REST)`)
	}
	if botLoginMatch("some-human", "devin-ai-integration[bot]") {
		t.Error(`botLoginMatch("some-human", "devin-ai-integration[bot]") = true, want false`)
	}
}

// TestActorKind covers the bot-vs-human discriminator across both API shapes:
// GraphQL __typename and REST user.type.
func TestActorKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typename, restType, want string
	}{
		{"Bot", "", "bot"},
		{"", "Bot", "bot"},
		{"User", "", "human"},
		{"", "User", "human"},
		{"", "", ""},
		{"bot", "", "bot"}, // case-insensitive
		{"", "user", "human"},
	}
	for _, c := range cases {
		if got := actorKind(c.typename, c.restType); got != c.want {
			t.Errorf("actorKind(%q, %q) = %q, want %q", c.typename, c.restType, got, c.want)
		}
	}
}

type githubTestResponse struct {
	stdout    string
	stderr    string
	wantStdin string
	code      int
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
			"GITHUB_TEST_WANT_STDIN="+response.wantStdin,
			fmt.Sprintf("GITHUB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestGitHubHelperProcess(t *testing.T) {
	if os.Getenv("GITHUB_TEST_HELPER") != "1" {
		return
	}

	if want := os.Getenv("GITHUB_TEST_WANT_STDIN"); want != "" {
		got, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read stdin: %v", err)
			os.Exit(1)
		}
		if string(got) != want {
			fmt.Fprintf(os.Stderr, "stdin = %q, want %q", string(got), want)
			os.Exit(1)
		}
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

// TestGetBotFindingsFiltersStaleThreadsByHeadSHA is the multi-reply regression
// test. On a real PR (robinhood-tracker#4), Devin posted findings on headA, the
// loop fixed and pushed headB, Devin re-reviewed headB with NEW findings
// (CHANGES_REQUESTED), and the loop fixed again — but the OLD threads from
// headA (not resolved, not outdated because the fix touched different lines)
// were still returned by GetBotFindings and got redundant "Addressed in <sha>"
// replies on every round (4 replies across 4 commits on the same thread).
//
// The fix: GetBotFindings must filter threads by originalCommit.oid == headSHA
// so only findings posted on the CURRENT head are live. Threads from older
// heads are stale (the loop already fixed them; Devin chose not to re-post on
// the new head) and must not drive fixes or receive replies.
//
// This test creates threads with explicit originalCommit { oid } fields:
//   - 2 threads on the current head (headSHA) → must be returned as findings
//   - 2 threads on an old head ("oldSHA123") → must be filtered out
//   - 1 thread with empty originalCommit.oid → must be returned (fail-safe:
//     a thread without commit metadata is treated as current-head, matching
//     the existing behavior for APIs that don't expose originalCommit)
func TestGetBotFindingsFiltersStaleThreadsByHeadSHA(t *testing.T) {
	t.Parallel()

	threads := []string{
		// Live bot threads on the CURRENT head — must be returned.
		threadWithCommit(101, false, false, botSlug, "internal/batch/download.go", 42, "🔴 **Bug on current head**", headSHA),
		threadWithCommit(102, false, false, botSlug, "internal/batch/path.go", 17, "🚩 **Medium on current head**", headSHA),
		// Live bot threads on an OLD head (not resolved, not outdated, but
		// originalCommit.oid != headSHA) — must be filtered out.
		threadWithCommit(201, false, false, botSlug, "internal/old/stale1.go", 10, "🔴 **Stale finding from old head**", "oldSHA123"),
		threadWithCommit(202, false, false, botSlug, "internal/old/stale2.go", 20, "🚩 **Stale medium from old head**", "oldSHA123"),
		// Live bot thread with empty originalCommit.oid — fail-safe: treat as
		// current-head (don't filter) so a host that doesn't expose the field
		// doesn't accidentally suppress all findings.
		threadWithCommit(301, false, false, botSlug, "internal/no/commit_meta.go", 5, "🔴 **No commit metadata**", ""),
	}

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(threads...),
	}), nil, "", "test/repo")

	findings, err := host.GetBotFindings(context.Background(), 7, headSHA, botUser)
	if err != nil {
		t.Fatalf("GetBotFindings() error = %v", err)
	}

	// 2 current-head threads + 1 empty-commit fail-safe thread = 3 findings.
	// The 2 stale threads from "oldSHA123" must be filtered out.
	if len(findings) != 3 {
		t.Fatalf("len(findings) = %d, want 3 (2 current-head + 1 empty-commit fail-safe; 2 stale filtered): %+v", len(findings), findings)
	}

	// Verify no stale thread (from oldSHA123) is in the results.
	for _, f := range findings {
		if f.ID == 201 || f.ID == 202 {
			t.Fatalf("stale thread from old head was not filtered out: %+v", f)
		}
	}

	// Verify the current-head and empty-commit findings are present.
	gotIDs := map[int64]bool{}
	for _, f := range findings {
		gotIDs[f.ID] = true
	}
	if !gotIDs[101] || !gotIDs[102] {
		t.Fatalf("current-head findings missing, got IDs: %v", gotIDs)
	}
	if !gotIDs[301] {
		t.Fatalf("empty-commit fail-safe finding missing, got IDs: %v", gotIDs)
	}
}

// TestGetBotFindingsHeadSHANormalization guards the two defensive normalizations
// in the stale-thread filter: the head-SHA comparison is case-insensitive
// (strings.EqualFold), and the headSHA argument is trimmed before comparison so
// a caller passing a whitespace-padded SHA still matches a clean originalCommit.
// oid. A regression that swaps EqualFold for == (mixed-case false-negative) or
// drops the headSHA trim (padded false-negative) would silently filter a
// current-head finding as stale, dropping a live finding.
func TestGetBotFindingsHeadSHANormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		commitOID string // originalCommit.oid on the thread
		argSHA    string // headSHA passed to GetBotFindings
	}{
		{
			// Thread's oid is upper-case but the head is lower-case: EqualFold
			// must still treat them as the same commit.
			name:      "mixed_case_oid_matches_via_EqualFold",
			commitOID: strings.ToUpper(headSHA),
			argSHA:    headSHA,
		},
		{
			// Caller passes a whitespace-padded head SHA: the trim in
			// GetBotFindings must normalize it so it matches the clean oid.
			name:      "whitespace_padded_headSHA_matches_after_trim",
			commitOID: headSHA,
			argSHA:    "  " + headSHA + "  ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			threads := []string{
				threadWithCommit(101, false, false, botSlug, "internal/batch/download.go", 42, "🔴 **Bug on current head**", tt.commitOID),
			}
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				graphqlThreadsKey("test/repo", 7): reviewThreadsResponse(threads...),
			}), nil, "", "test/repo")

			findings, err := host.GetBotFindings(context.Background(), 7, tt.argSHA, botUser)
			if err != nil {
				t.Fatalf("GetBotFindings() error = %v", err)
			}
			if len(findings) != 1 {
				t.Fatalf("len(findings) = %d, want 1 (current-head thread must not be filtered as stale): %+v", len(findings), findings)
			}
			if findings[0].ID != 101 {
				t.Fatalf("finding ID = %d, want 101", findings[0].ID)
			}
		})
	}
}
