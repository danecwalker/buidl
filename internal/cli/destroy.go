package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/ui"
)

// newDestroyCmd tears an environment down.
func newDestroyCmd(a *App) *cobra.Command {
	var (
		yes    bool
		force  bool
		dryRun bool
		stale  string
	)

	cmd := &cobra.Command{
		Use:          "destroy",
		SilenceUsage: true,
		Short:        "Tear down an environment",
		Long: `Remove a deployed app from the cluster.

A file with no environment overlays has one target: ` + "`buidl destroy`" + `
is enough. When overlays are declared, ` + "`-e`" + ` is required so this
cannot tear down the wrong one.

Preview environments live in a namespace of their own. destroy deletes that
namespace, which is how a pull request's app goes away when the PR closes.

Staging and production keep their namespace and any accessories. Only the
app objects (Deployment, Service, Ingress, and so on) are removed.

Examples:
  buidl destroy --yes
  buidl destroy -e preview --yes
  buidl destroy -e preview --dry-run
  buidl destroy -e preview --stale 7d --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			// When overlays exist, destroy must not inherit defaultEnvironment:
			// tearing down the wrong one is worse than asking. A file with no
			// environments has a single target, so -e would be ceremony.
			if a.opts.environment == "" && len(a.environments) > 0 {
				return fmt.Errorf("`destroy` requires -e/--env so it cannot target the wrong environment")
			}

			staleAfter, err := parseStaleDuration(stale)
			if err != nil {
				return err
			}

			// Production teardown is a real operation (leave the database, take
			// the app down) but it is never the default. --force is the extra
			// gate; --yes is still required in CI.
			if config.ProductionLike(a.cfg.Environment) && !force {
				return fmt.Errorf("refusing to destroy %s\n\nhint: pass --force --yes if you really mean to take this environment down", a.cfg.Environment)
			}

			slug := a.interpolationVars(a.git)["BUIDL_SLUG"]
			decision := deploy.DecideDestroy(a.cfg, slug)
			if decision.Scope == deploy.ScopeRefused {
				return fmt.Errorf("%s", decision.Reason)
			}

			if err := a.ensureClusterCredentials(cmd); err != nil {
				return err
			}

			target, err := a.target()
			if err != nil {
				return err
			}
			defer target.Close()

			req := deploy.DestroyRequest{
				Config:     a.cfg,
				Root:       a.root,
				Slug:       slug,
				DryRun:     dryRun,
				StaleAfter: staleAfter,
			}

			if staleAfter > 0 && !dryRun {
				// List first so the confirmation names the namespaces. A
				// confirm-after-delete would be theatre.
				preview, err := target.Destroy(ctx, deploy.DestroyRequest{
					Config:     a.cfg,
					Root:       a.root,
					Slug:       slug,
					DryRun:     true,
					StaleAfter: staleAfter,
				})
				if err != nil {
					return err
				}
				if preview.Mode == deploy.DestroyModeNone {
					a.reportDestroy(preview, false)
					return nil
				}
				if err := a.confirmStale(cmd, preview, staleAfter, yes); err != nil {
					return err
				}
			} else if !dryRun {
				if err := a.confirmDestroy(cmd, decision, yes); err != nil {
					return err
				}
			}

			outcome, err := target.Destroy(ctx, req)
			if err != nil {
				return err
			}
			a.reportDestroy(outcome, dryRun)
			return nil
		},
	}

	f := cmd.Flags()
	f.BoolVarP(&yes, "yes", "y", false, "skip confirmation prompts")
	f.BoolVar(&force, "force", false, "allow destroying a production-like environment")
	f.BoolVar(&dryRun, "dry-run", false, "show what would be deleted without deleting it")
	f.StringVar(&stale, "stale", "", "delete preview namespaces older than this (e.g. 7d, 24h)")

	return cmd
}

// confirmDestroy asks before tearing down a single environment.
func (a *App) confirmDestroy(cmd *cobra.Command, decision deploy.DestroyDecision, yes bool) error {
	if yes {
		return nil
	}
	if a.log.CI().Detected || a.log.Mode() != ui.ModePretty {
		return fmt.Errorf("`destroy` is irreversible; pass --yes to confirm")
	}

	ns := a.cfg.Deploy.Kubernetes.Namespace
	switch decision.Scope {
	case deploy.ScopeNamespace:
		fmt.Fprintf(cmd.OutOrStdout(),
			"About to delete preview namespace %q (cluster: %s).\nThis removes every object in that namespace.\n",
			ns, a.clusterDescription())
	default:
		fmt.Fprintf(cmd.OutOrStdout(),
			"About to remove %s from %s (namespace %s, cluster: %s).\nAccessories and the namespace are left alone.\n",
			a.cfg.App, a.cfg.Environment, ns, a.clusterDescription())
	}
	return a.confirm(cmd, "Continue?", "destroy cancelled")
}

// confirmStale asks before sweeping several preview namespaces.
func (a *App) confirmStale(cmd *cobra.Command, preview *deploy.DestroyOutcome, age time.Duration, yes bool) error {
	if yes {
		return nil
	}
	if a.log.CI().Detected || a.log.Mode() != ui.ModePretty {
		return fmt.Errorf("`destroy --stale` is irreversible; pass --yes to confirm")
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"About to delete %d preview namespace(s) older than %s:\n",
		len(preview.Namespaces), age)
	for _, ns := range preview.Namespaces {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", ns)
	}
	return a.confirm(cmd, "Continue?", "destroy cancelled")
}

// reportDestroy prints the teardown result.
func (a *App) reportDestroy(outcome *deploy.DestroyOutcome, dryRun bool) {
	a.log.EndStep()

	if outcome.Mode == deploy.DestroyModeNone {
		a.log.Success("nothing to destroy")
		return
	}

	if len(outcome.Changes) > 0 {
		rows := make([][]string, 0, len(outcome.Changes))
		for _, c := range outcome.Changes {
			status := "deleted"
			if dryRun {
				status = "would delete"
			}
			if c.Err != nil {
				status = "FAILED"
			}
			rows = append(rows, []string{status, c.Kind, c.Name, c.Impact})
		}
		a.log.Table([]string{"", "kind", "name", "effect"}, rows)
		a.log.Info("")
	}

	verb := "destroyed"
	if dryRun {
		verb = "would destroy"
	}
	switch outcome.Mode {
	case deploy.DestroyModeNamespace:
		a.log.Success("%s namespace %s", verb, outcome.Namespace)
	case deploy.DestroyModeStale:
		a.log.Success("%s %d preview namespace(s)", verb, len(outcome.Namespaces))
	default:
		a.log.Success("%s %s from %s", verb, a.cfg.App, outcome.Environment)
	}

	if a.log.Mode() == ui.ModeJSON {
		a.log.Fields("destroy complete", map[string]any{
			"environment": outcome.Environment,
			"namespace":   outcome.Namespace,
			"mode":        string(outcome.Mode),
			"deleted":     len(outcome.Changes),
			"dry_run":     dryRun,
		})
	}

	a.exportToCI(map[string]string{
		"namespace": outcome.Namespace,
		"mode":      string(outcome.Mode),
	})
}

// parseStaleDuration accepts Go durations plus the day/week units people
// actually write. time.ParseDuration does not understand "7d".
func parseStaleDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if n, ok := trailingUnit(s, 'd'); ok {
		if n < 1 {
			return 0, fmt.Errorf("invalid --stale %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if n, ok := trailingUnit(s, 'w'); ok {
		if n < 1 {
			return 0, fmt.Errorf("invalid --stale %q", s)
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --stale %q (want 7d, 24h, 90m)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--stale must be greater than zero")
	}
	return d, nil
}

func trailingUnit(s string, unit byte) (int, bool) {
	if len(s) < 2 || s[len(s)-1] != unit {
		return 0, false
	}
	// "7d" is ours; "7h" must stay a Go duration. Reject anything that
	// isn't a bare integer so "1.5d" does not silently truncate.
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return 0, false
	}
	return n, true
}
