// Package github implements scm.Host backed by the gh CLI.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
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

// botLoginSuffix is the type-marker REST glues onto a bot actor's login to
// fit it into the User namespace. GraphQL's Bot type does NOT carry it (the
// __typename field carries the type instead), so the same bot has two login
// strings across the two APIs. normalizeBotLogin strips it from both sides
// before comparison so a config in either form matches an API value in
// either form.
const botLoginSuffix = "[bot]"

// normalizeBotLogin strips a trailing "[bot]" (case-insensitive) and lowercases
// the rest, returning the bare app slug for a bot or the bare login for a
// human. GitHub reserves app slugs — a human cannot register a bot's slug — so
// slug equality is a safe identity match. Trimming the suffix on both sides
// means a config value "devin-ai-integration[bot]" (REST form) and a GraphQL
// author login "devin-ai-integration" (slug form) compare equal, and a
// slug-form config also matches a REST "[bot]" login.
func normalizeBotLogin(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(strings.ToLower(s), botLoginSuffix) {
		s = s[:len(s)-len(botLoginSuffix)]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// botLoginMatch reports whether apiLogin identifies the same actor as
// configuredLogin, normalizing the REST "[bot]" suffix on both sides. Use this
// instead of a bare strings.EqualFold against a bot login: GitHub's REST and
// GraphQL APIs return different login strings for the same bot (REST glues a
// "[bot]" type-marker into the login; GraphQL's Bot type does not), so an
// exact comparison silently never matches the GraphQL form — the root cause of
// the false-green bug where every bot thread was skipped and the verdict read
// as APPROVED with zero findings.
func botLoginMatch(apiLogin, configuredLogin string) bool {
	return strings.EqualFold(normalizeBotLogin(apiLogin), normalizeBotLogin(configuredLogin))
}

// actorKind returns "bot" if the API actor is a Bot-type actor, "human" if it
// is a User, or "" if the type is unknown/missing. GraphQL exposes the actor
// type as __typename; REST exposes it as user.type. This is the positive
// bot-vs-human discriminator GitHub designed for exactly this purpose —
// stronger than inferring from a "[bot]" suffix on the login, and it does not
// rely on app-slug reservation as the sole guarantee. Callers should require
// the actor to be a bot (actorKind == "bot") before honoring its findings as
// review-bot findings.
func actorKind(typename, restType string) string {
	t := strings.TrimSpace(typename)
	if t == "" {
		t = strings.TrimSpace(restType)
	}
	switch strings.ToLower(t) {
	case "bot":
		return "bot"
	case "user":
		return "human"
	}
	return ""
}

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
// and fetches every page. Without it a list read (PR review comments, issue
// comments, or reviews) silently stops at the first page (~30 items): a severe
// finding or the head-SHA review object that lands beyond page 1 would be
// dropped, and the review gate could then wrongly treat a changes-requested PR
// as approved.
//
// NOTE: for a JSON-array endpoint, `gh api --paginate` (without --slurp) emits
// each page as its OWN top-level array document concatenated back-to-back
// ([...][...]), not one merged array. Callers must stream-decode the output
// (see decodePaginatedArray), since a single json.Unmarshal fails once a second
// page exists.
func (h *Host) paginatedAPIArgs(suffix string) []string {
	return append(h.apiArgs(suffix), "--paginate")
}

// decodePaginatedArray flattens the multi-document output of a paginated
// `gh api` array read (see paginatedAPIArgs) into a single slice. Each page is a
// separate top-level JSON array, so it decodes the stream value-by-value and
// concatenates every page; a single-page (one array) response decodes as one
// value. An empty/whitespace-only body yields a nil slice.
func decodePaginatedArray[T any](out []byte) ([]T, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var items []T
	for {
		var page []T
		if err := dec.Decode(&page); err != nil {
			if errors.Is(err, io.EOF) {
				return items, nil
			}
			return nil, err
		}
		items = append(items, page...)
	}
}

type ghUser struct {
	Login string `json:"login"`
	// Type carries the actor type ("Bot" or "User") from the REST API. GraphQL
	// does not populate it (it uses __typename instead); see ghThreadAuthor.
	Type string `json:"type,omitempty"`
}

// reviewThreadsQuery reads the PR's review threads together with their
// resolution and outdated state so the read layer can keep only LIVE findings.
// GitHub re-anchors a bot's old inline comments onto the latest commit, so a
// REST read of pulls/{n}/comments reports already-addressed comments as live and
// the verdict never clears. The GraphQL reviewThreads API exposes the truth:
// isResolved (a human/bot resolved the thread) and isOutdated (the code the
// comment was anchored to has changed = effectively addressed). The first
// comment in a thread carries its author, anchor (path/line), and databaseId
// (the REST id needed to reply); replies follow.
//
// It is CURSOR-PAGINATED via $cursor / pageInfo: reviewThreads returns threads
// oldest-first and counts live, outdated, and resolved threads alike toward the
// first:100 window. On a busy PR a newest live severe finding could land past
// the first page, get truncated, and wrongly read as APPROVED, so GetBotFindings
// walks every page (after:$cursor) before filtering.
const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){pageInfo{hasNextPage endCursor}nodes{isResolved isOutdated comments(first:10){nodes{author{login __typename} databaseId path line originalLine body url originalCommit{oid}}}}}}}}`

// ghReviewThreadsResponse is the `gh api graphql` payload for reviewThreadsQuery.
type ghReviewThreadsResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []ghReviewThread `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

// ghReviewThread is one PR review thread: its resolution/outdated state plus the
// ordered comments it contains (the first is the anchoring comment).
type ghReviewThread struct {
	IsResolved bool `json:"isResolved"`
	IsOutdated bool `json:"isOutdated"`
	Comments   struct {
		Nodes []ghThreadComment `json:"nodes"`
	} `json:"comments"`
}

// ghThreadComment is a single comment node within a review thread. DatabaseID is
// the comment's REST id, used as ReviewComment.ID so the loop can reply to it.
// OriginalCommit.OID is the head SHA the thread was originally posted on, used by
// GetBotFindings to filter stale threads from older heads (see the headSHA filter).
type ghThreadComment struct {
	Author         ghThreadAuthor `json:"author"`
	DatabaseID     int64          `json:"databaseId"`
	Path           string         `json:"path"`
	Line           int            `json:"line"`
	OriginalLine   int            `json:"originalLine"`
	Body           string         `json:"body"`
	URL            string         `json:"url"`
	OriginalCommit struct {
		OID string `json:"oid"`
	} `json:"originalCommit"`
}

// ghThreadAuthor is the author of a GraphQL review-thread comment. GraphQL
// returns the actor type as __typename ("Bot" or "User"), NOT as a `type`
// field like REST does. The login for a Bot is the bare app slug (no "[bot]"
// suffix); see normalizeBotLogin for why both forms must compare equal.
type ghThreadAuthor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

// ownerName resolves the repo slug into its owner and name. When the slug is
// unknown it falls back to gh's {owner}/{repo} placeholders, which `gh api`
// (including graphql -F field values) fills from the working-dir repo — the
// graphql analogue of apiArgs()'s placeholder fallback.
func (h *Host) ownerName() (string, string) {
	owner, name, ok := strings.Cut(h.repo, "/")
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if !ok || owner == "" || name == "" {
		return "{owner}", "{repo}"
	}
	return owner, name
}

// reviewThreadsArgs builds the `gh api graphql` invocation that fetches one page
// of the PR's review threads (reviewThreadsQuery) bound to this host's repo and
// prNumber. cursor is the endCursor of the previous page ("" for the first page,
// where $cursor resolves to null = start from the beginning). It reuses the same
// CmdFactory as every other gh call; only the args differ.
//
// owner and name are passed with -f (raw string), not -F: -F magic-coerces an
// all-numeric value (e.g. a repo literally named "123") into a JSON number,
// which then fails to bind to the String! GraphQL variable. The integer number
// variable still uses -F so it binds to Int!.
func (h *Host) reviewThreadsArgs(prNumber int, cursor string) []string {
	owner, name := h.ownerName()
	args := []string{
		"api", "graphql",
		"-f", "query=" + reviewThreadsQuery,
		"-f", "owner=" + owner,
		"-f", "name=" + name,
		"-F", fmt.Sprintf("number=%d", prNumber),
	}
	if cursor != "" {
		args = append(args, "-f", "cursor="+cursor)
	}
	return args
}

// ghReview is a PR review object from `gh api repos/{owner}/{repo}/pulls/{n}/reviews`.
type ghReview struct {
	State       string `json:"state"`
	CommitID    string `json:"commit_id"`
	SubmittedAt string `json:"submitted_at"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
	User        ghUser `json:"user"`
}

// GetBotFindings returns botLogin's LIVE, file-scoped findings on the PR. It
// reads the PR's review threads via `gh api graphql` (reviewThreadsQuery) and
// keeps only threads that are still actionable:
//
//   - the thread's FIRST comment was authored by botLogin (replies are ignored);
//   - the thread is NOT resolved (isResolved==false) and NOT outdated
//     (isOutdated==false); and
//   - the thread is file-scoped (its anchor has a path).
//
// isOutdated is the primary liveness signal — more reliable than commit_id
// matching. GitHub re-anchors a bot's old inline comments onto the latest
// commit, so a REST commit_id read reports already-addressed comments as live;
// an outdated thread means the anchored code changed (the comment was
// effectively addressed) and a resolved thread means someone closed it. Both are
// excluded so a fixed-but-lingering comment no longer counts as a phantom
// finding and the post-PR review loop can converge.
//
// Top-level (issue) comments are not review threads and so never appear here:
// they carry no path, are review summaries rather than actionable file-scoped
// findings, and do not drive the verdict. Severity is parsed from each body.
func (h *Host) GetBotFindings(ctx context.Context, prNumber int, headSHA, botLogin string) ([]scm.ReviewComment, error) {
	botLogin = strings.TrimSpace(botLogin)
	if botLogin == "" {
		botLogin = DefaultBotLogin
	}

	// Fail-safe: without a head SHA we cannot confirm we are reading the current
	// commit's review state, so report no findings rather than risk surfacing a
	// stale thread. Mirrors GetReviewVerdict's empty-head short-circuit and keeps
	// the two consistent (and short-circuits before any gh round-trip). Trim once
	// here so the originalCommit.oid comparison below is symmetric with the
	// (also-trimmed) oid (a callers-can't-be-trusted defensive normalization).
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return nil, nil
	}

	// Walk every page of review threads before filtering. reviewThreads returns
	// threads oldest-first and counts resolved/outdated threads toward the
	// first:100 window, so on a busy PR a newest live severe finding can land past
	// page 1; reading only the first page could truncate it and wrongly read the
	// verdict as APPROVED. The empty-endCursor guard prevents an infinite loop if
	// the API ever reports hasNextPage:true without advancing the cursor.
	var threads []ghReviewThread
	cursor := ""
	for {
		out, err := h.cmd(ctx, "gh", h.reviewThreadsArgs(prNumber, cursor)...).Output()
		if err != nil {
			return nil, fmt.Errorf("gh api graphql reviewThreads: %w", err)
		}
		var resp ghReviewThreadsResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			return nil, fmt.Errorf("parse review threads: %w", err)
		}
		rt := resp.Data.Repository.PullRequest.ReviewThreads
		threads = append(threads, rt.Nodes...)
		if !rt.PageInfo.HasNextPage || strings.TrimSpace(rt.PageInfo.EndCursor) == "" {
			break
		}
		cursor = rt.PageInfo.EndCursor
	}

	findings := make([]scm.ReviewComment, 0, len(threads))
	for _, t := range threads {
		// A resolved thread (someone marked it done) or an outdated thread (the
		// anchored code changed = effectively addressed) is no longer live and must
		// not count toward the verdict — this is the convergence mechanism.
		if t.IsResolved || t.IsOutdated {
			continue
		}
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		first := t.Comments.Nodes[0]
		// Only the configured review bot's threads are findings. Match the
		// author by normalized login (handles the REST "[bot]" vs GraphQL
		// bare-slug inconsistency) AND positively assert the actor is a Bot —
		// defense in depth so a human who happens to share a slug (impossible
		// in practice since GitHub reserves app slugs, but asserted anyway)
		// can never inject findings. GraphQL carries the actor type as
		// __typename (Typename here); REST carries it as user.type.
		if actorKind(first.Author.Typename, "") != "bot" {
			continue
		}
		if !botLoginMatch(first.Author.Login, botLogin) {
			continue
		}
		// Filter stale findings from older heads: a (bot) thread whose
		// originalCommit.oid does not match the current headSHA was posted on a
		// previous commit the loop already fixed. This runs after the bot-identity
		// checks above because findings are exclusively bot threads — "stale
		// finding" only has meaning among the bot's own threads, so the head-SHA
		// scope belongs here, not before the author filter. GitHub only marks a
		// thread isOutdated when the anchored lines changed, so a fix that touched
		// different lines leaves the thread live — but it is stale (Devin chose not
		// to re-post it on the new head). Without this filter, stale threads drive
		// redundant fix rounds and get redundant "Addressed in <sha>" replies on
		// every push (observed on a real PR: 4 replies across 4 commits on one
		// thread). A thread with an empty originalCommit.oid (a host or API version
		// that doesn't expose the field) is treated as current-head (fail-safe:
		// don't suppress findings when the metadata is absent).
		//
		// Tradeoff: this assumes Devin re-posts still-applicable findings as NEW
		// threads on each head it reviews. If Devin instead reviews only the
		// inter-head diff and does NOT re-surface a still-valid finding whose
		// anchored lines were untouched, this filter silently drops a genuinely-
		// unaddressed finding and the loop could converge to APPROVED with a real
		// bug unfixed. The CI idle timeout / human escalation provide a backstop.
		// Empirically (observed across multiple repos), Devin posts fresh review
		// threads on each head it reviews, so this assumption holds in practice.
		if oid := strings.TrimSpace(first.OriginalCommit.OID); oid != "" && !strings.EqualFold(oid, headSHA) {
			continue
		}
		if strings.TrimSpace(first.Path) == "" {
			continue // not a file-scoped finding
		}
		findings = append(findings, scm.ReviewComment{
			ID:       first.DatabaseID,
			Path:     first.Path,
			Line:     pickLine(first.Line, first.OriginalLine),
			Body:     first.Body,
			Severity: severityFromBody(first.Body),
			URL:      first.URL,
		})
	}
	return findings, nil
}

// GetReviewVerdict derives botLogin's verdict for headSHA from the PR's review
// objects + its LIVE findings, never from a commit status check. It also returns
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
//   - CHANGES_REQUESTED: the bot reviewed headSHA and EITHER its MOST RECENT
//     review on the matching head is a native CHANGES_REQUESTED state, its
//     top-level body says it found potential issues, OR it left at least one
//     severe (high/medium) LIVE file-scoped finding.
//   - APPROVED: the bot reviewed headSHA, its most recent head review is native
//     APPROVED or its top-level body says "No Issues Found", and no severe LIVE
//     file-scoped findings remain.
//   - PENDING: the bot reviewed headSHA with a COMMENTED state whose body does
//     not carry a recognized clean/finding verdict yet.
//
// Only the MOST RECENT bot review targeting the head (by submitted_at) decides
// the native state: a bot that requested changes and then re-reviewed the same
// SHA as APPROVED must clear, so the states are not OR-ed across the head's
// review history. A native CHANGES_REQUESTED state is honored directly: a
// reviewer that uses GitHub's request-changes flow must not be treated as
// approved just because no inline body parsed as severe. Otherwise only live
// file-scoped findings drive CHANGES_REQUESTED: GetBotFindings already excludes
// resolved/outdated review threads, so a finding the bot left but whose code has
// since changed (or whose thread was resolved) no longer keeps the verdict
// not-green — which is what finally lets the loop converge to APPROVED.
func (h *Host) GetReviewVerdict(ctx context.Context, prNumber int, headSHA, botLogin string) (scm.ReviewVerdict, []scm.ReviewComment, error) {
	botLogin = strings.TrimSpace(botLogin)
	if botLogin == "" {
		botLogin = DefaultBotLogin
	}

	// Fail-safe: without a head SHA we cannot scope reviews to the current commit.
	// Rather than letting an empty SHA act as a wildcard that matches every review
	// in the PR's history, report not-yet-reviewed so the caller's grace window /
	// fail policy decides. This also keeps GetReviewVerdict and GetBotFindings
	// consistent (GetBotFindings likewise returns nothing on an empty head). Trim
	// once here so every downstream comparison — and the GetBotFindings call below —
	// uses the normalized value (a callers-can't-be-trusted defensive normalization).
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return scm.VerdictNone, nil, nil
	}

	reviewsOut, err := h.cmd(ctx, "gh", h.paginatedAPIArgs(fmt.Sprintf("pulls/%d/reviews", prNumber))...).Output()
	if err != nil {
		return "", nil, fmt.Errorf("gh api pulls reviews: %w", err)
	}
	reviews, err := decodePaginatedArray[ghReview](reviewsOut)
	if err != nil {
		return "", nil, fmt.Errorf("parse PR reviews: %w", err)
	}

	reviewedAny, reviewedHead, headChangesRequested := false, false, false
	headNativeApproved := false
	var latestHeadBody string
	// Track the most recent bot review targeting the head (by submitted_at) so its
	// state — not the OR of every state in the head's history — decides the native
	// verdict. Ties (or unparseable timestamps) fall back to API order, which is
	// already chronological, so the last matching review wins.
	var latestHeadAt time.Time
	var haveLatestHead bool
	for _, r := range reviews {
		// Same identity check as GetBotFindings: normalized login match plus a
		// positive Bot-type assertion. REST carries the type as user.type
		// (e.g. "Bot"); the login carries the "[bot]" suffix. Both signals are
		// checked so a non-bot review (a human's) is never counted as the
		// configured review bot's verdict even if logins collided.
		if actorKind("", r.User.Type) != "bot" {
			continue
		}
		if !botLoginMatch(r.User.Login, botLogin) {
			continue
		}
		reviewedAny = true
		if !sameCommitSHA(r.CommitID, headSHA) {
			continue
		}
		reviewedHead = true
		submittedAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(r.SubmittedAt))
		if haveLatestHead && submittedAt.Before(latestHeadAt) {
			continue
		}
		latestHeadAt = submittedAt
		haveLatestHead = true
		state := strings.TrimSpace(r.State)
		headChangesRequested = strings.EqualFold(state, "CHANGES_REQUESTED")
		headNativeApproved = strings.EqualFold(state, "APPROVED")
		latestHeadBody = r.Body
	}
	if !reviewedAny {
		return scm.VerdictNone, nil, nil
	}

	bodyVerdict := devinReviewBodyVerdict(latestHeadBody)
	if bodyVerdict == reviewBodyFindings {
		headChangesRequested = true
	}
	findings, err := h.GetBotFindings(ctx, prNumber, headSHA, botLogin)
	if err != nil {
		// A native CHANGES_REQUESTED on the head is authoritative from the review
		// state/body alone, so surface not-green even when the findings read fails.
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
	if headNativeApproved || bodyVerdict == reviewBodyClean {
		return scm.VerdictApproved, findings, nil
	}
	return scm.VerdictPending, findings, nil
}

func sameCommitSHA(reviewSHA, headSHA string) bool {
	reviewSHA = strings.ToLower(strings.TrimSpace(reviewSHA))
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	if reviewSHA == "" || headSHA == "" {
		return false
	}
	if reviewSHA == headSHA {
		return true
	}
	if len(reviewSHA) < 7 || len(headSHA) < 7 {
		return false
	}
	if len(reviewSHA) > len(headSHA) {
		return strings.HasPrefix(reviewSHA, headSHA)
	}
	if len(headSHA) > len(reviewSHA) {
		return strings.HasPrefix(headSHA, reviewSHA)
	}
	return false
}

type reviewBodyVerdict int

const (
	reviewBodyUnknown reviewBodyVerdict = iota
	reviewBodyClean
	reviewBodyFindings
)

var (
	reviewBodyNoIssuesRE = regexp.MustCompile(`(?i)\bno\s+issues?\s+found\b`)
	reviewBodyFindingsRE = regexp.MustCompile(`(?i)\bfound\s+[1-9][0-9]*\s+potential\s+issues?\b`)
)

func devinReviewBodyVerdict(body string) reviewBodyVerdict {
	body = strings.Join(strings.Fields(body), " ")
	if body == "" {
		return reviewBodyUnknown
	}
	if reviewBodyFindingsRE.MatchString(body) {
		return reviewBodyFindings
	}
	if reviewBodyNoIssuesRE.MatchString(body) {
		return reviewBodyClean
	}
	return reviewBodyUnknown
}

// ReplyToReviewComment posts a threaded reply under an existing PR review comment
// (identified by its REST/database id) via
// `gh api repos/{owner}/{repo}/pulls/{n}/comments/{id}/replies` (a POST: gh
// switches to POST automatically once a -f field is supplied). It reuses
// apiArgs() for the repo-scoped path, mirroring every other gh-api call. The
// review loop calls it best-effort to acknowledge addressed findings; callers
// should gate on Capabilities().Reviews.
func (h *Host) ReplyToReviewComment(ctx context.Context, prNumber int, commentID int64, body string) error {
	args := append(h.apiArgs(fmt.Sprintf("pulls/%d/comments/%d/replies", prNumber, commentID)), "-f", "body="+body)
	if out, err := h.cmd(ctx, "gh", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("gh api reply to review comment: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

const (
	severityHigh   = "high"
	severityMedium = "medium"
	severityLow    = "low"
)

// severityFromBody parses a coarse severity bucket from a bot finding body.
// "high" requires an EXPLICIT marker — only the 🔴 emoji (Devin uses it for
// bug/high); 🚩 marks medium. The keyword fallback deliberately never escalates
// to high: a bare-word heuristic false-positives on negated or contextual
// mentions ("this is not a bug", "no critical issues" would otherwise both
// register as high), so an unmarked body is treated conservatively as medium
// (still severe enough to gate) unless it is explicitly low/nit.
//
// Keyword matching for low/nit is word-boundary based rather than substring
// based: a plain strings.Contains for "low" also fires on "allow", "follow",
// "below", "flow", and "slow", which would silently downgrade a severe unmarked
// finding to low and drop it from the verdict.
func severityFromBody(body string) string {
	switch {
	case strings.Contains(body, "🔴"):
		return severityHigh
	case strings.Contains(body, "🚩"):
		return severityMedium
	}
	words := bodyWordSet(strings.ToLower(body))
	switch {
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

func pickLine(line, originalLine int) int {
	if line > 0 {
		return line
	}
	return originalLine
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
