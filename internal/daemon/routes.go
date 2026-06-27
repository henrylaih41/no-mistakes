package daemon

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// resolveRoute resolves the effective base (PR base / upstream) and fork URLs
// for a run from the repo's LOCAL routes.
//
// Precedence:
//  1. An explicit route name (from a no-mistakes.route push-option) selects
//     that named route.
//  2. Otherwise the repo's DefaultRoute, when set.
//  3. Otherwise the repo record's own UpstreamURL/ForkURL — the implicit
//     default route, which keeps absent-config behavior byte-identical to the
//     pre-routes path.
//
// An explicit route name that names no defined route is an error so a bad
// selector FAILS THE RUN FAST (surfaced to the pusher) rather than silently
// falling back to a different target.
//
// Routes are read only from the local gate database (never from the pushed
// branch or any in-repo file), so a pushed feature branch can neither define
// nor redirect a route; the push-option only selects one by name.
func resolveRoute(d *db.DB, repo *db.Repo, routeName string) (baseURL, forkURL string, err error) {
	routeName = strings.TrimSpace(routeName)
	if routeName != "" {
		r, err := d.GetRoute(repo.ID, routeName)
		if err != nil {
			return "", "", fmt.Errorf("look up route %q: %w", routeName, err)
		}
		if r == nil {
			return "", "", fmt.Errorf("unknown route %q: no such local route (define it with: no-mistakes route add %s --base <url>)", routeName, routeName)
		}
		return r.BaseURL, r.ForkURL, nil
	}

	if def := strings.TrimSpace(repo.DefaultRoute); def != "" {
		r, err := d.GetRoute(repo.ID, def)
		if err != nil {
			return "", "", fmt.Errorf("look up default route %q: %w", def, err)
		}
		if r != nil {
			return r.BaseURL, r.ForkURL, nil
		}
		// The default route was removed out from under us. Fall back to the
		// repo record rather than failing every push; remove clears a dangling
		// default, so this is only a defensive path.
		slog.Warn("default route not found; falling back to repo record", "repo_id", repo.ID, "default_route", def)
	}

	return repo.UpstreamURL, repo.ForkURL, nil
}

// resolveRepoRoute returns a copy of repo with its UpstreamURL/ForkURL replaced
// by the route resolved from routeName. The copy is what the pipeline steps
// read (Repo.UpstreamURL, Repo.ForkURL, Repo.PushURL), so push/host/rebase work
// unchanged off the resolved fields. The stored record is never mutated.
func (m *RunManager) resolveRepoRoute(repo *db.Repo, routeName string) (*db.Repo, error) {
	baseURL, forkURL, err := resolveRoute(m.db, repo, routeName)
	if err != nil {
		return nil, err
	}
	slog.Info("route resolved", "repo_id", repo.ID, "route", routeName, "base", safeurl.Redact(baseURL), "fork", safeurl.Redact(forkURL))
	effective := *repo
	effective.UpstreamURL = baseURL
	effective.ForkURL = forkURL
	return &effective, nil
}
