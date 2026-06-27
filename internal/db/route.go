package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// Route is a named, locally-defined push target for a repo: a PR base
// (upstream) URL and an optional fork push URL. Routes let one clone raise PRs
// to different targets, chosen per push via a no-mistakes.route push-option,
// without re-init or a second clone.
//
// Routes are stored only in the local gate database, never read from a pushed
// branch or any in-repo file, so a contributor cannot define or redirect one
// through a push. A push-option only SELECTS a pre-defined local route by name.
type Route struct {
	RepoID    string
	Name      string
	BaseURL   string
	ForkURL   string
	CreatedAt int64
}

// PushURL returns the URL that should receive branch updates for this route:
// the fork when set, otherwise the base. Mirrors Repo.PushURL.
func (r *Route) PushURL() string {
	if r == nil {
		return ""
	}
	if strings.TrimSpace(r.ForkURL) != "" {
		return r.ForkURL
	}
	return r.BaseURL
}

// AddRoute inserts a named route for a repo. The (repo_id, name) primary key
// makes inserting a duplicate name fail.
func (d *DB) AddRoute(repoID, name, baseURL, forkURL string) (*Route, error) {
	r := &Route{
		RepoID:    repoID,
		Name:      name,
		BaseURL:   strings.TrimSpace(baseURL),
		ForkURL:   strings.TrimSpace(forkURL),
		CreatedAt: now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO routes (repo_id, name, base_url, fork_url, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.RepoID, r.Name, r.BaseURL, nullableString(r.ForkURL), r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("add route: %w", err)
	}
	return r, nil
}

// GetRoute returns a repo's route by name, or nil when none is defined.
func (d *DB) GetRoute(repoID, name string) (*Route, error) {
	r := &Route{}
	err := d.sql.QueryRow(
		`SELECT repo_id, name, base_url, COALESCE(fork_url, ''), created_at FROM routes WHERE repo_id = ? AND name = ?`,
		repoID, name,
	).Scan(&r.RepoID, &r.Name, &r.BaseURL, &r.ForkURL, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get route: %w", err)
	}
	return r, nil
}

// ListRoutes returns a repo's routes ordered by name.
func (d *DB) ListRoutes(repoID string) ([]*Route, error) {
	rows, err := d.sql.Query(
		`SELECT repo_id, name, base_url, COALESCE(fork_url, ''), created_at FROM routes WHERE repo_id = ? ORDER BY name`,
		repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	defer rows.Close()

	var routes []*Route
	for rows.Next() {
		r := &Route{}
		if err := rows.Scan(&r.RepoID, &r.Name, &r.BaseURL, &r.ForkURL, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan route: %w", err)
		}
		routes = append(routes, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate routes: %w", err)
	}
	return routes, nil
}

// RemoveRoute deletes a repo's route by name. It reports whether a row was
// removed so callers can distinguish a successful delete from "no such route".
func (d *DB) RemoveRoute(repoID, name string) (bool, error) {
	res, err := d.sql.Exec(`DELETE FROM routes WHERE repo_id = ? AND name = ?`, repoID, name)
	if err != nil {
		return false, fmt.Errorf("remove route: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("remove route: %w", err)
	}
	return n > 0, nil
}
