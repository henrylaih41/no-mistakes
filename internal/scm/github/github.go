// Package github implements scm.Host backed by the gh CLI.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to GitHub through the gh CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	repo         string // "owner/name" slug for --repo; empty when unknown
	forkOwner    string // fork owner for cross-repository PR heads
}

// New builds a Host. cliAvailable reports whether the gh binary is
// resolvable on the caller's PATH (possibly overridden by env). repo is the
// "owner/name" slug; when set it is passed via --repo to every PR/run command
// so they resolve the right repository regardless of the process working
// directory. The daemon runs from a fixed, non-repo working dir, so without
// this gh cannot infer the repo (or branch) and fails on every poll.
func New(cmd CmdFactory, cliAvailable func() bool, repo string) *Host {
	return &Host{cmd: cmd, cliAvailable: cliAvailable, repo: strings.TrimSpace(repo)}
}

// NewWithFork builds a Host that opens PRs on repo using forkRepo as the head
// repository owner. forkRepo is an "owner/name" slug; only the owner is needed
// because gh pr create expects --head <owner>:<branch>.
func NewWithFork(cmd CmdFactory, cliAvailable func() bool, repo, forkRepo string) *Host {
	h := New(cmd, cliAvailable, repo)
	h.forkOwner = repoOwner(forkRepo)
	return h
}

// RepoSlug extracts the "owner/name" identifier from a GitHub remote or PR URL.
// It supports https URLs, scp-style ssh URLs (git@github.com:owner/name.git),
// ssh:// URLs, and longer paths such as PR links (the leading two path segments
// are used). It returns "" when the input has no owner/name pair.
func RepoSlug(remoteURL string) string {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return ""
	}
	raw = strings.TrimSuffix(raw, ".git")

	// Reduce raw to the path portion after the host.
	switch {
	case strings.Contains(raw, "://"):
		rest := raw[strings.Index(raw, "://")+len("://"):]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return ""
		}
		raw = rest[slash+1:]
	case strings.Contains(raw, ":"):
		// scp-style ssh: [user@]host:owner/name
		raw = raw[strings.IndexByte(raw, ':')+1:]
	}

	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	owner, name := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

// repoArgs returns the --repo flag pair when the slug is known, so gh commands
// resolve the right repository regardless of the process working directory.
func (h *Host) repoArgs() []string {
	if h.repo == "" {
		return nil
	}
	return []string{"--repo", h.repo}
}

func (h *Host) headRef(branch string) string {
	if h.forkOwner == "" {
		return branch
	}
	return h.forkOwner + ":" + branch
}

func repoOwner(slug string) string {
	owner, _, ok := strings.Cut(strings.TrimSpace(slug), "/")
	if !ok {
		return ""
	}
	return strings.TrimSpace(owner)
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitHub }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true, Reviews: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("gh CLI is not installed")
	}
	if err := h.cmd(ctx, "gh", "auth", "status").Run(); err != nil {
		return errors.New("gh CLI is not authenticated")
	}
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"pr", "list", "--head", branch}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--base", base)
	}
	args = append(args, h.repoArgs()...)
	jsonFields := "number,url"
	if h.forkOwner != "" {
		jsonFields = "number,url,headRefName,headRepositoryOwner"
	}
	args = append(args, "--state", "open", "--json", jsonFields)
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var prs []struct {
		Number              int    `json:"number"`
		URL                 string `json:"url"`
		HeadRefName         string `json:"headRefName"`
		HeadRepositoryOwner *struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
	}
	if err := json.Unmarshal(out, &prs); err != nil || len(prs) == 0 {
		return nil, nil
	}
	for _, candidate := range prs {
		if !h.matchesHead(candidate.HeadRefName, candidate.HeadRepositoryOwner, branch) {
			continue
		}
		pr := &scm.PR{URL: strings.TrimSpace(candidate.URL)}
		if candidate.Number > 0 {
			pr.Number = fmt.Sprintf("%d", candidate.Number)
		} else if num, nerr := scm.ExtractPRNumber(pr.URL); nerr == nil {
			pr.Number = num
		}
		if pr.URL == "" {
			return nil, nil
		}
		return pr, nil
	}
	return nil, nil
}

func (h *Host) matchesHead(headRefName string, owner *struct {
	Login string `json:"login"`
}, branch string) bool {
	if h.forkOwner == "" {
		return true
	}
	if strings.TrimSpace(headRefName) != "" && headRefName != branch {
		return false
	}
	if owner == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(owner.Login), h.forkOwner)
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	args := append([]string{"pr", "create",
		"--head", h.headRef(branch),
		"--base", base,
	}, h.repoArgs()...)
	args = append(args, "--title", content.Title, "--body", content.Body)
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	url := strings.TrimSpace(string(out))
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := pr.Number
	if id == "" {
		id = pr.URL
	}
	args := append([]string{"pr", "edit", id}, h.repoArgs()...)
	args = append(args, "--title", content.Title, "--body", content.Body)
	cmd := h.cmd(ctx, "gh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gh pr edit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	args := append([]string{"pr", "view", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "state", "--jq", ".state")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return normalizePRState(strings.TrimSpace(string(out))), nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	args := append([]string{"pr", "checks", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "name,state,bucket,completedAt")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no checks reported") {
			return nil, nil
		}
		return nil, fmt.Errorf("gh pr checks: %w", err)
	}
	var raw []struct {
		Name        string `json:"name"`
		State       string `json:"state"`
		Bucket      string `json:"bucket"`
		CompletedAt string `json:"completedAt"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse CI checks: %w", err)
	}
	checks := make([]scm.Check, 0, len(raw))
	for _, r := range raw {
		var completedAt time.Time
		if r.CompletedAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, r.CompletedAt); parseErr == nil {
				completedAt = parsed
			}
		}
		checks = append(checks, scm.Check{Name: r.Name, Bucket: normalizeCheckBucket(r.Bucket, r.State), CompletedAt: completedAt})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	args := append([]string{"pr", "view", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "mergeable", "--jq", ".mergeable")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view mergeable: %w", err)
	}
	return normalizeMergeableState(strings.TrimSpace(string(out))), nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, _ *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	targets := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		name = normalizeRunName(name)
		if name != "" {
			targets[name] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return "", nil
	}
	args := []string{"run", "list", "--branch", branch}
	if strings.TrimSpace(headSHA) != "" {
		args = append(args, "--commit", strings.TrimSpace(headSHA))
	}
	args = append(args, h.repoArgs()...)
	args = append(args,
		"--status", "failure",
		"--limit", "20",
		"--json", "databaseId,headSha,name,displayTitle,workflowName",
	)
	listCmd := h.cmd(ctx, "gh", args...)
	listOut, err := listCmd.Output()
	if err != nil {
		return "", nil
	}
	var runs []githubRun
	if err := json.Unmarshal(listOut, &runs); err != nil {
		return "", nil
	}
	for _, run := range runs {
		if !runMatchesTargets(ctx, h, run, targets) {
			continue
		}
		viewArgs := append([]string{"run", "view", fmt.Sprintf("%d", run.DatabaseID)}, h.repoArgs()...)
		viewArgs = append(viewArgs, "--log-failed")
		viewCmd := h.cmd(ctx, "gh", viewArgs...)
		out, err := viewCmd.Output()
		if err != nil {
			continue
		}
		logs := strings.TrimSpace(string(out))
		if logs != "" {
			return logs, nil
		}
	}
	return "", nil
}

// DefaultBotLogin is the GitHub account a reviewing bot posts under when the
// caller does not override it. It mirrors the config default; the two layers are
// independent (the github package does not import config), so the literal is
// duplicated deliberately.
const DefaultBotLogin = "devin-ai-integration[bot]"

// apiArgs builds the args for `gh api repos/{owner}/{repo}/<suffix>`. The repo
// slug is embedded in the path because `gh api` (unlike `gh pr`) does not accept
// --repo; this is the gh-api equivalent of repoArgs(). When the slug is unknown
// the {owner}/{repo} placeholder lets gh resolve the repo from its working dir.
func (h *Host) apiArgs(suffix string) []string {
	repo := h.repo
	if repo == "" {
		repo = "{owner}/{repo}"
	}
	return []string{"api", "repos/" + repo + "/" + suffix}
}

// paginatedAPIArgs is apiArgs plus --paginate, so gh follows the Link headers
// and returns every page merged into a single JSON array. Without it a list read
// (PR review comments, issue comments, or reviews) silently stops at the first
// page (~30 items): a severe finding or the head-SHA review object that lands
// beyond page 1 would be dropped, and the review gate could then wrongly treat a
// changes-requested PR as approved.
func (h *Host) paginatedAPIArgs(suffix string) []string {
	return append(h.apiArgs(suffix), "--paginate")
}

type ghUser struct {
	Login string `json:"login"`
}

// ghReviewComment is a PR review (inline, file-scoped) comment from
// `gh api repos/{owner}/{repo}/pulls/{n}/comments`.
type ghReviewComment struct {
	Path             string `json:"path"`
	Line             int    `json:"line"`
	OriginalLine     int    `json:"original_line"`
	Body             string `json:"body"`
	CommitID         string `json:"commit_id"`
	OriginalCommitID string `json:"original_commit_id"`
	HTMLURL          string `json:"html_url"`
	User             ghUser `json:"user"`
}

// ghIssueComment is a top-level (non-file-scoped) PR comment from
// `gh api repos/{owner}/{repo}/issues/{n}/comments`. These carry no commit SHA.
type ghIssueComment struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    ghUser `json:"user"`
}

// ghReview is a PR review object from `gh api repos/{owner}/{repo}/pulls/{n}/reviews`.
type ghReview struct {
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	HTMLURL  string `json:"html_url"`
	User     ghUser `json:"user"`
}

// GetBotFindings returns botLogin's findings on the PR scoped to headSHA. It
// reads inline (file-scoped) review comments and keeps only those tied to
// headSHA via original_commit_id or commit_id, plus the bot's top-level review
// comments (which carry no SHA and so cannot be scoped — they are the most
// recent review round's summary in practice). Severity is parsed from each body.
func (h *Host) GetBotFindings(ctx context.Context, prNumber int, headSHA, botLogin string) ([]scm.ReviewComment, error) {
	botLogin = strings.TrimSpace(botLogin)
	if botLogin == "" {
		botLogin = DefaultBotLogin
	}

	inlineOut, err := h.cmd(ctx, "gh", h.paginatedAPIArgs(fmt.Sprintf("pulls/%d/comments", prNumber))...).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api pulls comments: %w", err)
	}
	var inline []ghReviewComment
	if err := json.Unmarshal(inlineOut, &inline); err != nil {
		return nil, fmt.Errorf("parse PR review comments: %w", err)
	}

	findings := make([]scm.ReviewComment, 0, len(inline))
	for _, c := range inline {
		if !strings.EqualFold(strings.TrimSpace(c.User.Login), botLogin) {
			continue
		}
		if !commentMatchesHead(c.CommitID, c.OriginalCommitID, headSHA) {
			continue
		}
		findings = append(findings, scm.ReviewComment{
			Path:     c.Path,
			Line:     pickLine(c.Line, c.OriginalLine),
			Body:     c.Body,
			Severity: severityFromBody(c.Body),
			CommitID: firstNonEmpty(c.OriginalCommitID, c.CommitID),
			URL:      c.HTMLURL,
		})
	}

	issueOut, err := h.cmd(ctx, "gh", h.paginatedAPIArgs(fmt.Sprintf("issues/%d/comments", prNumber))...).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api issues comments: %w", err)
	}
	var issue []ghIssueComment
	if err := json.Unmarshal(issueOut, &issue); err != nil {
		return nil, fmt.Errorf("parse PR issue comments: %w", err)
	}
	for _, c := range issue {
		if !strings.EqualFold(strings.TrimSpace(c.User.Login), botLogin) {
			continue
		}
		// Top-level comments have no Path/Line/CommitID: they are not tied to a
		// file or a SHA. They are included for completeness but do not drive the
		// verdict (see GetReviewVerdict).
		findings = append(findings, scm.ReviewComment{
			Body:     c.Body,
			Severity: severityFromBody(c.Body),
			URL:      c.HTMLURL,
		})
	}
	return findings, nil
}

// GetReviewVerdict derives botLogin's verdict for headSHA from the PR's review
// objects + inline findings, never from a commit status check. It also returns
// the findings it read so callers do not need a second GetBotFindings round-trip
// (the verdict already required them).
//
// Why derive: Devin posts PR reviews with state=COMMENTED (never
// CHANGES_REQUESTED) and publishes no commit status check, so a status rollup is
// empty even when it has flagged blocking issues. It also re-reviews per commit,
// so there can be several review objects per head SHA.
//
// Derivation:
//   - NONE: the bot has never reviewed the PR.
//   - PENDING: the bot has reviewed before but not yet headSHA.
//   - CHANGES_REQUESTED: the bot reviewed headSHA and EITHER posted a native
//     CHANGES_REQUESTED review state on the matching head, OR left at least one
//     severe (high/medium) file-scoped finding tied to headSHA.
//   - APPROVED: the bot reviewed headSHA with no CHANGES_REQUESTED state and no
//     severe file-scoped findings.
//
// A native CHANGES_REQUESTED state is honored directly: a reviewer that uses
// GitHub's request-changes flow must not be treated as approved just because no
// inline body parsed as severe. Only file-scoped (inline) findings otherwise
// drive CHANGES_REQUESTED; a top-level review summary is informational. REST
// does not expose thread resolution, so every returned finding is unresolved.
func (h *Host) GetReviewVerdict(ctx context.Context, prNumber int, headSHA, botLogin string) (scm.ReviewVerdict, []scm.ReviewComment, error) {
	botLogin = strings.TrimSpace(botLogin)
	if botLogin == "" {
		botLogin = DefaultBotLogin
	}

	reviewsOut, err := h.cmd(ctx, "gh", h.paginatedAPIArgs(fmt.Sprintf("pulls/%d/reviews", prNumber))...).Output()
	if err != nil {
		return "", nil, fmt.Errorf("gh api pulls reviews: %w", err)
	}
	var reviews []ghReview
	if err := json.Unmarshal(reviewsOut, &reviews); err != nil {
		return "", nil, fmt.Errorf("parse PR reviews: %w", err)
	}

	head := strings.TrimSpace(headSHA)
	reviewedAny, reviewedHead, headChangesRequested := false, false, false
	for _, r := range reviews {
		if !strings.EqualFold(strings.TrimSpace(r.User.Login), botLogin) {
			continue
		}
		reviewedAny = true
		if head == "" || strings.EqualFold(strings.TrimSpace(r.CommitID), head) {
			reviewedHead = true
			if strings.EqualFold(strings.TrimSpace(r.State), "CHANGES_REQUESTED") {
				headChangesRequested = true
			}
		}
	}
	if !reviewedAny {
		return scm.VerdictNone, nil, nil
	}

	findings, err := h.GetBotFindings(ctx, prNumber, headSHA, botLogin)
	if err != nil {
		// A native CHANGES_REQUESTED on the head is authoritative from the review
		// state alone, so surface not-green even when the findings read fails.
		if headChangesRequested {
			return scm.VerdictChangesRequested, nil, nil
		}
		return "", nil, err
	}
	if !reviewedHead {
		return scm.VerdictPending, findings, nil
	}
	if headChangesRequested {
		return scm.VerdictChangesRequested, findings, nil
	}
	for _, f := range findings {
		if f.Path == "" {
			continue // top-level summary, not a file-scoped finding
		}
		if f.Severity == severityHigh || f.Severity == severityMedium {
			return scm.VerdictChangesRequested, findings, nil
		}
	}
	return scm.VerdictApproved, findings, nil
}

const (
	severityHigh   = "high"
	severityMedium = "medium"
	severityLow    = "low"
)

// severityFromBody parses a coarse severity bucket from a bot finding body.
// Emoji markers win (Devin uses 🔴 for bug/high and 🚩 for analysis/medium),
// then a whole-word keyword heuristic. Keyword matching is word-boundary based
// rather than substring based: a plain strings.Contains for "low" also fires on
// "allow", "follow", "below", "flow", and "slow", which would silently downgrade
// a severe unmarked finding to low and drop it from the verdict. An unrecognized
// body defaults to medium so an unmarked inline finding is treated
// conservatively as severe.
func severityFromBody(body string) string {
	switch {
	case strings.Contains(body, "🔴"):
		return severityHigh
	case strings.Contains(body, "🚩"):
		return severityMedium
	}
	words := bodyWordSet(strings.ToLower(body))
	switch {
	case words["bug"] || words["high"] || words["critical"]:
		return severityHigh
	case words["medium"]:
		return severityMedium
	case words["low"] || words["nit"]:
		return severityLow
	default:
		return severityMedium
	}
}

// bodyWordSet splits text into whole words (maximal runs of letters and digits),
// so keyword lookups are boundary-aware instead of matching arbitrary
// substrings of longer words.
func bodyWordSet(text string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		set[w] = true
	}
	return set
}

// commentMatchesHead reports whether an inline comment is tied to headSHA via
// either original_commit_id (the commit it was first made against) or commit_id
// (GitHub's running update). An empty headSHA cannot be scoped, so it matches.
func commentMatchesHead(commitID, originalCommitID, headSHA string) bool {
	head := strings.TrimSpace(headSHA)
	if head == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(originalCommitID), head) ||
		strings.EqualFold(strings.TrimSpace(commitID), head)
}

func pickLine(line, originalLine int) int {
	if line > 0 {
		return line
	}
	return originalLine
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

type githubRun struct {
	DatabaseID   int    `json:"databaseId"`
	HeadSHA      string `json:"headSha"`
	Name         string `json:"name"`
	DisplayTitle string `json:"displayTitle"`
	WorkflowName string `json:"workflowName"`
}

type githubRunView struct {
	Jobs []githubRunJob `json:"jobs"`
}

type githubRunJob struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
}

func runMatchesTargets(ctx context.Context, h *Host, run githubRun, targets map[string]struct{}) bool {
	for _, candidate := range []string{run.Name, run.DisplayTitle, run.WorkflowName} {
		if _, ok := targets[normalizeRunName(candidate)]; ok {
			return true
		}
	}
	if run.DatabaseID == 0 {
		return false
	}
	viewArgs := append([]string{"run", "view", fmt.Sprintf("%d", run.DatabaseID)}, h.repoArgs()...)
	viewArgs = append(viewArgs, "--json", "jobs")
	viewCmd := h.cmd(ctx, "gh", viewArgs...)
	out, err := viewCmd.Output()
	if err != nil {
		return false
	}
	var payload githubRunView
	if err := json.Unmarshal(out, &payload); err != nil {
		return false
	}
	for _, job := range payload.Jobs {
		if !isFailedJob(job) {
			continue
		}
		if _, ok := targets[normalizeRunName(job.Name)]; ok {
			return true
		}
	}
	return false
}

func isFailedJob(job githubRunJob) bool {
	state := strings.ToUpper(strings.TrimSpace(job.Conclusion))
	if state == "" {
		state = strings.ToUpper(strings.TrimSpace(job.Status))
	}
	switch state {
	case "FAILURE", "FAILED", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return true
	default:
		return false
	}
}

func normalizeRunName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "OPEN":
		return scm.PRStateOpen
	case "MERGED":
		return scm.PRStateMerged
	case "CLOSED":
		return scm.PRStateClosed
	default:
		return scm.PRState(raw)
	}
}

func normalizeMergeableState(raw string) scm.MergeableState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MERGEABLE":
		return scm.MergeableOK
	case "CONFLICTING":
		return scm.MergeableConflict
	case "UNKNOWN", "":
		return scm.MergeablePending
	default:
		return scm.MergeableState(raw)
	}
}

func normalizeCheckBucket(bucket, state string) scm.CheckBucket {
	if normalized := scm.CheckBucket(strings.TrimSpace(bucket)); normalized != "" {
		return normalized
	}

	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return scm.CheckBucketPass
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return scm.CheckBucketFail
	case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "EXPECTED":
		return scm.CheckBucketPending
	case "CANCELLED":
		return scm.CheckBucketCancel
	case "SKIPPED", "NEUTRAL", "STALE":
		return scm.CheckBucketSkip
	default:
		return ""
	}
}
