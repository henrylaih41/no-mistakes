package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// registerRouteTestRepo creates an isolated repo record at the current git root
// so the route commands (which resolve the repo by path) operate on it. It
// returns the resolved repo ID.
func registerRouteTestRepo(t *testing.T) string {
	t.Helper()
	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New: %v", err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatalf("find git root: %v", err)
	}
	repo, err := d.InsertRepo(gitRoot, "https://github.com/legacy/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return repo.ID
}

func TestRouteValidName(t *testing.T) {
	for _, ok := range []string{"parent", "fork-pr", "v2.1", "a_b", "X9"} {
		if !validRouteName(ok) {
			t.Errorf("validRouteName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "-leading", ".dot", "has space", "../escape", "a/b", "a:b"} {
		if validRouteName(bad) {
			t.Errorf("validRouteName(%q) = true, want false", bad)
		}
	}
}

func TestRouteCommandLifecycle(t *testing.T) {
	setupTestRepo(t)
	repoID := registerRouteTestRepo(t)

	// add (with fork): base is GitHub, fork is GitHub → valid.
	out, err := executeCmd("route", "add", "parent",
		"--base", "https://github.com/parent/repo.git",
		"--fork-url", "https://github.com/fork/repo.git")
	if err != nil {
		t.Fatalf("route add parent: %v\n%s", err, out)
	}
	if !strings.Contains(out, "parent") {
		t.Fatalf("route add output missing name: %q", out)
	}

	// add a second, fork-less route.
	if _, err := executeCmd("route", "add", "self", "--base", "https://github.com/fork/repo.git"); err != nil {
		t.Fatalf("route add self: %v", err)
	}

	// duplicate name rejected.
	if _, err := executeCmd("route", "add", "parent", "--base", "https://github.com/other/repo.git"); err == nil {
		t.Fatal("expected duplicate route name to fail")
	}

	// invalid slug rejected.
	if _, err := executeCmd("route", "add", "bad name", "--base", "https://github.com/x/y.git"); err == nil {
		t.Fatal("expected invalid route name to fail")
	}

	// invalid fork routing rejected (fork must be GitHub when set).
	if _, err := executeCmd("route", "add", "mixed",
		"--base", "https://github.com/parent/repo.git",
		"--fork-url", "https://gitlab.com/fork/repo.git"); err == nil {
		t.Fatal("expected non-GitHub fork routing to fail validation")
	}

	// list shows both routes.
	out, err = executeCmd("route", "list")
	if err != nil {
		t.Fatalf("route list: %v", err)
	}
	if !strings.Contains(out, "parent") || !strings.Contains(out, "self") {
		t.Fatalf("route list missing routes: %q", out)
	}

	// set-default to an existing route.
	if _, err := executeCmd("route", "set-default", "self"); err != nil {
		t.Fatalf("route set-default self: %v", err)
	}
	// set-default to a missing route fails.
	if _, err := executeCmd("route", "set-default", "nope"); err == nil {
		t.Fatal("expected set-default on unknown route to fail")
	}

	assertRouteDefault(t, repoID, "self")

	// removing the default route clears the default.
	if _, err := executeCmd("route", "remove", "self"); err != nil {
		t.Fatalf("route remove self: %v", err)
	}
	assertRouteDefault(t, repoID, "")

	// removing a missing route fails.
	if _, err := executeCmd("route", "remove", "self"); err == nil {
		t.Fatal("expected remove on absent route to fail")
	}
}

func assertRouteDefault(t *testing.T, repoID, want string) {
	t.Helper()
	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New: %v", err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	repo, err := d.GetRepo(repoID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if repo.DefaultRoute != want {
		t.Fatalf("default route = %q, want %q", repo.DefaultRoute, want)
	}
}
