package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func openRouteTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestResolveRouteLegacyFallback is the backward-compat proof: with no routes
// defined and no route push-option, resolution returns the repo record's own
// UpstreamURL/ForkURL verbatim — byte-identical to the pre-routes path.
func TestResolveRouteLegacyFallback(t *testing.T) {
	d := openRouteTestDB(t)
	repo, err := d.InsertRepoWithFork("/work/repo", "https://github.com/parent/repo.git", "https://github.com/fork/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	base, fork, _, err := resolveRoute(d, repo, "")
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if base != repo.UpstreamURL {
		t.Fatalf("base = %q, want record upstream %q", base, repo.UpstreamURL)
	}
	if fork != repo.ForkURL {
		t.Fatalf("fork = %q, want record fork %q", fork, repo.ForkURL)
	}
}

func TestResolveRouteNamed(t *testing.T) {
	d := openRouteTestDB(t)
	repo, _ := d.InsertRepo("/work/repo", "https://github.com/parent/repo.git", "main")
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/repo.git", "https://github.com/fork/repo.git"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	base, fork, _, err := resolveRoute(d, repo, "parent")
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if base != "https://github.com/parent/repo.git" {
		t.Fatalf("base = %q", base)
	}
	if fork != "https://github.com/fork/repo.git" {
		t.Fatalf("fork = %q", fork)
	}
}

func TestResolveRouteDefault(t *testing.T) {
	d := openRouteTestDB(t)
	repo, _ := d.InsertRepo("/work/repo", "https://github.com/legacy/repo.git", "main")
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/repo.git", "https://github.com/fork/repo.git"); err != nil {
		t.Fatalf("add route: %v", err)
	}
	repo, err := d.UpdateRepoDefaultRoute(repo.ID, "parent")
	if err != nil {
		t.Fatalf("set default route: %v", err)
	}

	// No explicit route → the default route is used, not the legacy record.
	base, fork, _, err := resolveRoute(d, repo, "")
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if base != "https://github.com/parent/repo.git" {
		t.Fatalf("base = %q, want default route base", base)
	}
	if fork != "https://github.com/fork/repo.git" {
		t.Fatalf("fork = %q, want default route fork", fork)
	}
}

// TestResolveRouteExplicitOverridesDefault proves an explicit selector wins
// over the configured default route.
func TestResolveRouteExplicitOverridesDefault(t *testing.T) {
	d := openRouteTestDB(t)
	repo, _ := d.InsertRepo("/work/repo", "https://github.com/legacy/repo.git", "main")
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/repo.git", "https://github.com/fork/repo.git"); err != nil {
		t.Fatalf("add parent route: %v", err)
	}
	if _, err := d.AddRoute(repo.ID, "self", "https://github.com/fork/repo.git", ""); err != nil {
		t.Fatalf("add self route: %v", err)
	}
	repo, err := d.UpdateRepoDefaultRoute(repo.ID, "self")
	if err != nil {
		t.Fatalf("set default route: %v", err)
	}

	base, fork, _, err := resolveRoute(d, repo, "parent")
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if base != "https://github.com/parent/repo.git" || fork != "https://github.com/fork/repo.git" {
		t.Fatalf("explicit route not honored: base=%q fork=%q", base, fork)
	}
}

func TestResolveRouteUnknownFailsFast(t *testing.T) {
	d := openRouteTestDB(t)
	repo, _ := d.InsertRepo("/work/repo", "https://github.com/parent/repo.git", "main")

	_, _, _, err := resolveRoute(d, repo, "nope")
	if err == nil {
		t.Fatal("expected unknown route to fail fast")
	}
	if !strings.Contains(err.Error(), "unknown route") {
		t.Fatalf("error = %v, want it to mention unknown route", err)
	}
}

// TestResolveRouteDanglingDefaultFallsBack covers the defensive path where a
// default route was removed: resolution falls back to the legacy record rather
// than failing every push.
func TestResolveRouteDanglingDefaultFallsBack(t *testing.T) {
	d := openRouteTestDB(t)
	repo, _ := d.InsertRepo("/work/repo", "https://github.com/legacy/repo.git", "main")
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/repo.git", ""); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := d.UpdateRepoDefaultRoute(repo.ID, "parent"); err != nil {
		t.Fatalf("set default route: %v", err)
	}
	// Remove the route directly, leaving a dangling default on the in-memory copy.
	if _, err := d.RemoveRoute(repo.ID, "parent"); err != nil {
		t.Fatalf("remove route: %v", err)
	}
	repo.DefaultRoute = "parent"

	base, fork, _, err := resolveRoute(d, repo, "")
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if base != "https://github.com/legacy/repo.git" || fork != "" {
		t.Fatalf("dangling default did not fall back to record: base=%q fork=%q", base, fork)
	}
}

// TestResolveRouteEffectiveName proves the effective route name reported back
// for persistence: the explicit selector, the resolved default route name, or
// "" when resolution falls through to the repo record (legacy or dangling
// default). Persisting the default route name is what keeps a default-route
// push target-stable across a later default change.
func TestResolveRouteEffectiveName(t *testing.T) {
	d := openRouteTestDB(t)
	repo, _ := d.InsertRepo("/work/repo", "https://github.com/legacy/repo.git", "main")
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/repo.git", ""); err != nil {
		t.Fatalf("add route: %v", err)
	}

	if _, _, name, err := resolveRoute(d, repo, "parent"); err != nil || name != "parent" {
		t.Fatalf("explicit route effective name = %q, err=%v; want %q", name, err, "parent")
	}

	if _, _, name, err := resolveRoute(d, repo, ""); err != nil || name != "" {
		t.Fatalf("legacy fallback effective name = %q, err=%v; want empty", name, err)
	}

	withDefault, err := d.UpdateRepoDefaultRoute(repo.ID, "parent")
	if err != nil {
		t.Fatalf("set default route: %v", err)
	}
	if _, _, name, err := resolveRoute(d, withDefault, ""); err != nil || name != "parent" {
		t.Fatalf("default route effective name = %q, err=%v; want %q", name, err, "parent")
	}

	withDefault.DefaultRoute = "gone"
	if _, _, name, err := resolveRoute(d, withDefault, ""); err != nil || name != "" {
		t.Fatalf("dangling default effective name = %q, err=%v; want empty", name, err)
	}
}
