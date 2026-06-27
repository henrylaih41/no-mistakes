package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/spf13/cobra"
)

// routeNameRE constrains route names to a safe slug: a leading letter or digit
// followed by letters, digits, '.', '_' or '-'. This is what both `route add`
// and the no-mistakes.route push-option accept.
var routeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// validRouteName reports whether name is a valid route slug.
func validRouteName(name string) bool {
	return routeNameRE.MatchString(name)
}

func newRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Manage local push routes (per-push PR targets)",
		Long: "Local named routes let one clone raise PRs to different targets (for example\n" +
			"your fork or the parent), chosen per push, without re-init or a second clone.\n" +
			"A route is a PR base (upstream) URL plus an optional fork push URL. Select one\n" +
			"on a push with: git push no-mistakes <branch> -o no-mistakes.route=<name>.\n\n" +
			"Routes are stored only in the local gate database — never read from a pushed\n" +
			"branch or any in-repo file — so a contributor cannot define or redirect a route\n" +
			"through a push. Routes generalize the single `init --fork-url` setting.",
	}

	cmd.AddCommand(newRouteAddCmd())
	cmd.AddCommand(newRouteListCmd())
	cmd.AddCommand(newRouteRemoveCmd())
	cmd.AddCommand(newRouteSetDefaultCmd())

	return cmd
}

func newRouteAddCmd() *cobra.Command {
	var baseURL string
	var forkURL string

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a local route (a PR base URL and optional fork push URL)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("route.add", func() error {
				name := strings.TrimSpace(args[0])
				if !validRouteName(name) {
					return fmt.Errorf("invalid route name %q: use letters, digits, '.', '_' or '-' (must start with a letter or digit)", args[0])
				}
				if strings.TrimSpace(baseURL) == "" {
					return fmt.Errorf("route add: --base must not be empty")
				}
				if err := gate.ValidateRoute(baseURL, forkURL); err != nil {
					return fmt.Errorf("route add: %w", err)
				}

				_, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := findRepo(d)
				if err != nil {
					return err
				}

				existing, err := d.GetRoute(repo.ID, name)
				if err != nil {
					return fmt.Errorf("check existing route: %w", err)
				}
				if existing != nil {
					return fmt.Errorf("route %q already exists (remove it first or pick another name)", name)
				}

				route, err := d.AddRoute(repo.ID, name, baseURL, forkURL)
				if err != nil {
					return fmt.Errorf("add route: %w", err)
				}

				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "  %s route %s added\n", sGreen.Render("✓"), sBold.Render(route.Name))
				printRouteDetail(cmd, route)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&baseURL, "base", "", "PR base (upstream) URL the route opens PRs against")
	cmd.Flags().StringVar(&forkURL, "fork-url", "", "optional fork remote URL to push branches to while opening PRs against --base")
	_ = cmd.MarkFlagRequired("base")
	return cmd
}

func newRouteListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List local routes for the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("route.list", func() error {
				_, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := findRepo(d)
				if err != nil {
					return err
				}

				routes, err := d.ListRoutes(repo.ID)
				if err != nil {
					return fmt.Errorf("list routes: %w", err)
				}

				w := cmd.OutOrStdout()
				if len(routes) == 0 {
					fmt.Fprintf(w, "  %s\n", sDim.Render("no routes defined; pushes use the default target:"))
					printDefaultTarget(cmd, repo)
					fmt.Fprintln(w)
					fmt.Fprintf(w, "  %s\n", sDim.Render("Add one with:"))
					fmt.Fprintf(w, "  %s\n", sBold.Render("no-mistakes route add <name> --base <url> [--fork-url <url>]"))
					return nil
				}

				for _, route := range routes {
					isDefault := route.Name == repo.DefaultRoute
					fmt.Fprintf(w, "  %s %s%s\n", sCyan.Render("●"), sBold.Render(route.Name), defaultMarker(isDefault))
					printRouteDetail(cmd, route)
				}

				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sDim.Render("default target when no route is selected:"))
				if repo.DefaultRoute == "" {
					printDefaultTarget(cmd, repo)
				} else {
					fmt.Fprintf(w, "  %s  route %s\n", sDim.Render("target"), sBold.Render(repo.DefaultRoute))
				}
				return nil
			})
		},
	}
}

func newRouteRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a local route",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("route.remove", func() error {
				name := strings.TrimSpace(args[0])

				_, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := findRepo(d)
				if err != nil {
					return err
				}

				removed, err := d.RemoveRoute(repo.ID, name)
				if err != nil {
					return fmt.Errorf("remove route: %w", err)
				}
				if !removed {
					return fmt.Errorf("no such route %q", name)
				}

				// Clear a dangling default so resolution never points at a route
				// that no longer exists.
				if repo.DefaultRoute == name {
					if _, err := d.UpdateRepoDefaultRoute(repo.ID, ""); err != nil {
						return fmt.Errorf("clear default route: %w", err)
					}
				}

				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "  %s route %s removed\n", sGreen.Render("✓"), sBold.Render(name))
				return nil
			})
		},
	}
}

func newRouteSetDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-default <name>",
		Short: "Set the route used when a push selects none",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("route.set-default", func() error {
				name := strings.TrimSpace(args[0])

				_, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := findRepo(d)
				if err != nil {
					return err
				}

				route, err := d.GetRoute(repo.ID, name)
				if err != nil {
					return fmt.Errorf("look up route: %w", err)
				}
				if route == nil {
					return fmt.Errorf("no such route %q (add it first with: no-mistakes route add %s --base <url>)", name, name)
				}

				if _, err := d.UpdateRepoDefaultRoute(repo.ID, name); err != nil {
					return fmt.Errorf("set default route: %w", err)
				}

				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "  %s default route set to %s\n", sGreen.Render("✓"), sBold.Render(name))
				return nil
			})
		},
	}
}

func defaultMarker(isDefault bool) string {
	if isDefault {
		return " " + sDim.Render("(default)")
	}
	return ""
}

// printRouteDetail renders a route's base and optional fork. Fork URLs are
// redacted, and the base is redacted when a fork is present, mirroring how init
// and eject treat fork-routed remotes.
func printRouteDetail(cmd *cobra.Command, route *db.Route) {
	w := cmd.OutOrStdout()
	base := route.BaseURL
	if strings.TrimSpace(route.ForkURL) != "" {
		base = safeurl.Redact(base)
	}
	fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  base"), base)
	if strings.TrimSpace(route.ForkURL) != "" {
		fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  fork"), safeurl.Redact(route.ForkURL))
	}
}

// printDefaultTarget renders the repo record's own upstream/fork — the implicit
// default route used when no named route applies.
func printDefaultTarget(cmd *cobra.Command, repo *db.Repo) {
	w := cmd.OutOrStdout()
	base := repo.UpstreamURL
	if strings.TrimSpace(repo.ForkURL) != "" {
		base = safeurl.Redact(base)
	}
	fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  base"), base)
	if strings.TrimSpace(repo.ForkURL) != "" {
		fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  fork"), safeurl.Redact(repo.ForkURL))
	}
}
