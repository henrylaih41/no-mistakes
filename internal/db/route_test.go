package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestRouteAddGetListRemove(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/project", "git@github.com:parent/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Absent route → nil.
	got, err := d.GetRoute(repo.ID, "parent")
	if err != nil {
		t.Fatalf("get absent route: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for absent route, got %+v", got)
	}

	route, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/project.git", "https://github.com/fork/project.git")
	if err != nil {
		t.Fatalf("add route: %v", err)
	}
	if route.PushURL() != "https://github.com/fork/project.git" {
		t.Fatalf("push url = %q, want fork URL", route.PushURL())
	}

	got, err = d.GetRoute(repo.ID, "parent")
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if got == nil {
		t.Fatal("expected route after add")
	}
	if got.BaseURL != "https://github.com/parent/project.git" {
		t.Fatalf("base url = %q", got.BaseURL)
	}
	if got.ForkURL != "https://github.com/fork/project.git" {
		t.Fatalf("fork url = %q", got.ForkURL)
	}

	// A route with no fork: PushURL falls back to the base.
	if _, err := d.AddRoute(repo.ID, "self", "https://github.com/fork/project.git", ""); err != nil {
		t.Fatalf("add fork-less route: %v", err)
	}
	self, err := d.GetRoute(repo.ID, "self")
	if err != nil {
		t.Fatalf("get fork-less route: %v", err)
	}
	if self.ForkURL != "" {
		t.Fatalf("fork url = %q, want empty", self.ForkURL)
	}
	if self.PushURL() != self.BaseURL {
		t.Fatalf("push url = %q, want base %q", self.PushURL(), self.BaseURL)
	}

	routes, err := d.ListRoutes(repo.ID)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("len routes = %d, want 2", len(routes))
	}
	// Ordered by name: "parent" then "self".
	if routes[0].Name != "parent" || routes[1].Name != "self" {
		t.Fatalf("route order = %q, %q; want parent, self", routes[0].Name, routes[1].Name)
	}

	removed, err := d.RemoveRoute(repo.ID, "parent")
	if err != nil {
		t.Fatalf("remove route: %v", err)
	}
	if !removed {
		t.Fatal("expected removed=true for existing route")
	}
	removed, err = d.RemoveRoute(repo.ID, "parent")
	if err != nil {
		t.Fatalf("remove absent route: %v", err)
	}
	if removed {
		t.Fatal("expected removed=false for absent route")
	}
}

func TestRemoveRouteAndClearDefault(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/project", "git@github.com:parent/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/project.git", ""); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := d.AddRoute(repo.ID, "other", "https://github.com/other/project.git", ""); err != nil {
		t.Fatalf("add other route: %v", err)
	}
	if _, err := d.UpdateRepoDefaultRoute(repo.ID, "parent"); err != nil {
		t.Fatalf("set default route: %v", err)
	}

	// Removing the default route clears the dangling default atomically.
	removed, err := d.RemoveRouteAndClearDefault(repo.ID, "parent")
	if err != nil {
		t.Fatalf("remove route and clear default: %v", err)
	}
	if !removed {
		t.Fatal("expected removed=true for existing route")
	}
	got, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.DefaultRoute != "" {
		t.Fatalf("default route = %q, want cleared", got.DefaultRoute)
	}

	// Removing an absent route reports removed=false and is a no-op.
	removed, err = d.RemoveRouteAndClearDefault(repo.ID, "parent")
	if err != nil {
		t.Fatalf("remove absent route: %v", err)
	}
	if removed {
		t.Fatal("expected removed=false for absent route")
	}

	// Removing a non-default route leaves an unrelated default untouched.
	if _, err := d.UpdateRepoDefaultRoute(repo.ID, "other"); err != nil {
		t.Fatalf("set default route to other: %v", err)
	}
	if _, err := d.AddRoute(repo.ID, "extra", "https://github.com/extra/project.git", ""); err != nil {
		t.Fatalf("add extra route: %v", err)
	}
	removed, err = d.RemoveRouteAndClearDefault(repo.ID, "extra")
	if err != nil {
		t.Fatalf("remove extra route: %v", err)
	}
	if !removed {
		t.Fatal("expected removed=true for extra route")
	}
	got, err = d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.DefaultRoute != "other" {
		t.Fatalf("default route = %q, want other preserved", got.DefaultRoute)
	}
}

func TestRouteDuplicateNameRejected(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:parent/project.git", "main")
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/project.git", ""); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/other/project.git", ""); err == nil {
		t.Fatal("expected duplicate route name to fail")
	}
}

func TestRouteCascadeOnRepoDelete(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:parent/project.git", "main")
	if _, err := d.AddRoute(repo.ID, "parent", "https://github.com/parent/project.git", ""); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	routes, err := d.ListRoutes(repo.ID)
	if err != nil {
		t.Fatalf("list routes after delete: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("expected routes to cascade-delete, got %d", len(routes))
	}
}

func TestRepoDefaultRouteRoundTrip(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:parent/project.git", "main")
	if repo.DefaultRoute != "" {
		t.Fatalf("fresh default route = %q, want empty", repo.DefaultRoute)
	}

	updated, err := d.UpdateRepoDefaultRoute(repo.ID, "parent")
	if err != nil {
		t.Fatalf("set default route: %v", err)
	}
	if updated.DefaultRoute != "parent" {
		t.Fatalf("default route = %q, want parent", updated.DefaultRoute)
	}

	got, err := d.GetRepoByPath("/home/user/project")
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if got.DefaultRoute != "parent" {
		t.Fatalf("default route after get = %q, want parent", got.DefaultRoute)
	}

	cleared, err := d.UpdateRepoDefaultRoute(repo.ID, "")
	if err != nil {
		t.Fatalf("clear default route: %v", err)
	}
	if cleared.DefaultRoute != "" {
		t.Fatalf("default route after clear = %q, want empty", cleared.DefaultRoute)
	}
}

func TestOpenCreatesRoutesTable(t *testing.T) {
	d := openTestDB(t)
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM routes").Scan(&count); err != nil {
		t.Fatalf("routes table missing: %v", err)
	}
	if !hasColumn(t, d, "repos", "default_route") {
		t.Fatal("repos.default_route column missing from fresh schema")
	}
}

func TestOpenMigratesReposDefaultRouteColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	// A pre-routes repos table: fork_url present, default_route absent.
	if _, err := legacyDB.Exec(`
		CREATE TABLE repos (
			id TEXT PRIMARY KEY,
			working_path TEXT NOT NULL UNIQUE,
			upstream_url TEXT NOT NULL,
			fork_url TEXT,
			default_branch TEXT NOT NULL DEFAULT 'main',
			created_at INTEGER NOT NULL
		);
		INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at)
		VALUES ('repo-1', '/work/repo', 'git@github.com:parent/repo.git', 'main', 123);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy repos table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if !hasColumn(t, d, "repos", "default_route") {
		t.Fatal("expected migrated default_route column")
	}
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM routes").Scan(&count); err != nil {
		t.Fatalf("expected routes table created on migration: %v", err)
	}

	repo, err := d.GetRepo("repo-1")
	if err != nil {
		t.Fatalf("get migrated repo: %v", err)
	}
	if repo == nil {
		t.Fatal("expected migrated repo")
	}
	if repo.DefaultRoute != "" {
		t.Fatalf("default route = %q, want empty", repo.DefaultRoute)
	}

	// Routes are usable against a migrated repo.
	if _, err := d.AddRoute("repo-1", "parent", "https://github.com/parent/repo.git", ""); err != nil {
		t.Fatalf("add route on migrated repo: %v", err)
	}
	updated, err := d.UpdateRepoDefaultRoute("repo-1", "parent")
	if err != nil {
		t.Fatalf("set default route on migrated repo: %v", err)
	}
	if updated.DefaultRoute != "parent" {
		t.Fatalf("default route after update = %q, want parent", updated.DefaultRoute)
	}
}
