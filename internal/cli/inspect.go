package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/deploy/kubernetes"
)

// newStatusCmd reports what is currently live.
func newStatusCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what is currently deployed",
		Long: `Report the live release, its health, and its instances.

This is the first command to run during an incident: it answers what is running,
which commit it came from, who deployed it, and which instances are unhealthy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if err := a.ensureClusterCredentials(cmd); err != nil {
				return err
			}

			target, err := a.target()
			if err != nil {
				return err
			}
			defer target.Close()

			st, err := target.Status(ctx, deploy.Request{Config: a.cfg, Root: a.root})
			if err != nil {
				return err
			}

			a.renderStatus(st)
			return nil
		},
	}
	return cmd
}

// renderStatus prints a status report.
func (a *App) renderStatus(st *deploy.Status) {
	if a.log.Mode() == "json" {
		a.log.Fields("status", map[string]any{
			"environment": st.Environment,
			"release":     st.Release,
			"digest":      st.Digest,
			"ready":       st.Ready,
			"desired":     st.Desired,
			"available":   st.Available,
			"url":         st.URL,
			"git_sha":     st.GitSHA,
		})
		return
	}

	// With maxUnavailable: 0 the old instances keep the workload "available" while
	// a new release fails to start, so availability alone is not health. Saying
	// "healthy" next to a stuck rollout would be the one thing this line must not
	// do.
	health := "healthy"
	switch {
	case !st.Available:
		health = "DEGRADED"
	case st.Updated > 0 && st.Updated != st.Desired:
		health = "rollout incomplete"
	}

	deployedAgo := ""
	if !st.DeployedAt.IsZero() {
		deployedAgo = humanAge(time.Since(st.DeployedAt)) + " ago"
	}

	a.log.KeyValues([][2]string{
		{"environment", st.Environment},
		{"release", orDash(st.Release)},
		{"image", orDash(st.Image)},
		{"instances", fmt.Sprintf("%d/%d ready (%s)", st.Ready, st.Desired, health)},
		{"commit", st.GitSHA},
		{"deployed by", st.DeployedBy},
		{"deployed", deployedAgo},
		{"url", st.URL},
	})

	// A rollout in progress is worth stating explicitly rather than leaving the
	// user to infer it from mismatched counts.
	if st.Updated > 0 && st.Updated != st.Desired {
		a.log.Warn("a rollout is in progress: %d/%d instances updated", st.Updated, st.Desired)
	}

	for _, cond := range st.Conditions {
		a.log.Warn("%s", cond)
	}

	if len(st.Pods) == 0 {
		return
	}

	// Group by release so a mid-rollout or blue-green state is legible: seeing two
	// releases side by side is the whole point during an incident.
	byRelease := map[string]int{}
	for _, p := range st.Pods {
		byRelease[p.Release]++
	}

	rows := make([][]string, 0, len(st.Pods))
	for _, p := range st.Pods {
		releaseLabel := p.Release
		if releaseLabel == st.Release {
			releaseLabel += " (live)"
		}
		rows = append(rows, []string{
			p.Name,
			orDash(releaseLabel),
			p.Phase,
			yesNo(p.Ready),
			fmt.Sprintf("%d", p.Restarts),
			humanAge(p.Age),
			orDash(p.Node),
			truncate(p.Message, 32),
		})
	}

	a.log.Info("")
	a.log.Info("Instances")
	a.log.Table([]string{"instance", "release", "phase", "ready", "restarts", "age", "node", "message"}, rows)

	if len(byRelease) > 1 {
		a.log.Warn("%d releases are running at once; a rollout or cutover may be incomplete", len(byRelease))
	}

	// Point at the next useful command rather than leaving the user to guess.
	unhealthy := 0
	for _, p := range st.Pods {
		if !p.Ready {
			unhealthy++
		}
	}
	if unhealthy > 0 {
		a.log.Info("")
		a.log.Info("%d instance(s) not ready; inspect with:", unhealthy)
		a.log.Info("  buidl logs -e %s", st.Environment)
	}
}

// newReleasesCmd lists deploy history.
func newReleasesCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "releases",
		Aliases: []string{"history"},
		Short:   "List deploy history",
		Long: `List the releases available in this environment, newest first.

History is read from the cluster's own revision records, so it is accurate even
for deploys made from another machine or another CI run. Any listed release can
be passed to ` + "`buidl rollback --to`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if err := a.ensureClusterCredentials(cmd); err != nil {
				return err
			}

			target, err := a.target()
			if err != nil {
				return err
			}
			defer target.Close()

			releases, err := target.Releases(ctx, deploy.Request{Config: a.cfg, Root: a.root})
			if err != nil {
				return err
			}
			if len(releases) == 0 {
				a.log.Info("no releases found for %s", a.cfg.Environment)
				return nil
			}

			rows := make([][]string, 0, len(releases))
			for _, r := range releases {
				live := ""
				if r.Live {
					live = "*"
				}
				age := ""
				if !r.CreatedAt.IsZero() {
					age = humanAge(time.Since(r.CreatedAt))
				}
				rows = append(rows, []string{
					live,
					r.ID,
					fmt.Sprintf("%d", r.Revision),
					shortSHA(r.GitSHA),
					truncate(r.GitBranch, 20),
					orDash(r.DeployedBy),
					age,
				})
			}
			a.log.Table([]string{"", "release", "rev", "commit", "branch", "by", "age"}, rows)
			return nil
		},
	}
	return cmd
}

// newLogsCmd streams application logs.
func newLogsCmd(a *App) *cobra.Command {
	var (
		follow    bool
		tail      int64
		since     time.Duration
		releaseID string
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Stream application logs",
		Long: `Stream logs from every instance of the live release.

Lines from multiple instances are interleaved and prefixed with the instance they
came from.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if err := a.ensureClusterCredentials(cmd); err != nil {
				return err
			}

			target, err := a.target()
			if err != nil {
				return err
			}
			defer target.Close()

			return target.Logs(ctx, deploy.LogRequest{
				Config:  a.cfg,
				Follow:  follow,
				Tail:    tail,
				Since:   since,
				Release: releaseID,
				Out:     cmd.OutOrStdout(),
			})
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "F", false, "stream new log lines as they arrive")
	cmd.Flags().Int64VarP(&tail, "tail", "n", 100, "number of recent lines to show (-1 for all)")
	cmd.Flags().DurationVar(&since, "since", 0, "only show logs newer than this (e.g. 10m)")
	cmd.Flags().StringVar(&releaseID, "release", "", "show logs for a specific release")

	return cmd
}

// newManifestCmd prints the rendered manifests.
func newManifestCmd(a *App) *cobra.Command {
	var digest string

	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Print the Kubernetes manifests buidl would apply",
		Long: `Render the manifests for an environment and print them as YAML.

Use this to review what buidl generates, to commit the output for a GitOps
workflow, or to hand it to another tool:

  buidl manifest -e production | kubectl apply -f -`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if err := a.requireConfig(ctx); err != nil {
				return err
			}

			secretValues, err := a.resolveSecrets()
			if err != nil {
				return err
			}

			rel := a.newRelease("")
			if digest != "" {
				rel.Digest = digest
			} else {
				// Rendering needs a digest; a placeholder keeps this command usable
				// before any image exists.
				rel.Digest = placeholderDigest
			}

			if a.cfg.Deploy.Target != "kubernetes" {
				return fmt.Errorf("`manifest` is only supported for the kubernetes target")
			}

			// Deliberately not a.target(): that loads a kubeconfig and builds
			// clients, so the documented GitOps use — `buidl manifest -e
			// production | kubectl apply -f -`, whose whole point is not needing
			// cluster access — failed on any machine without one.
			renderer := kubernetes.NewRenderer(a.cfg, a.log)

			out, err := renderer.ManifestYAML(a.deployRequest(rel, secretValues, false, false))
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}

	cmd.Flags().StringVar(&digest, "digest", "", "render with this image digest")
	return cmd
}

// newConfigCmd inspects the resolved configuration.
func newConfigCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the resolved configuration",
	}

	show := &cobra.Command{
		Use:   "show",
		Short: "Print the fully resolved config for an environment",
		Long: `Print the configuration after environment overlays, variable interpolation,
and defaults have been applied.

This is the ground truth for "why did it deploy that": it shows the values buidl
actually used, not what the file literally contains.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if err := a.requireConfig(ctx); err != nil {
				return err
			}

			out, err := yaml.Marshal(a.cfg)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "# resolved from %s for environment %q\n%s", a.path, a.cfg.Environment, out)
			return nil
		},
	}

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate the config for one or all environments",
		Long: `Validate the configuration.

Without -e, every declared environment is validated, which is what a CI lint job
wants: a typo in a rarely deployed environment is caught before someone tries to
deploy it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if a.opts.environment != "" {
				if err := a.requireConfig(ctx); err != nil {
					return err
				}
				a.log.Success("%s is valid for environment %q", a.path, a.cfg.Environment)
				return nil
			}

			// Load once to discover the environment list.
			if err := a.validateAllEnvironments(ctx); err != nil {
				return err
			}
			return nil
		},
	}

	envs := &cobra.Command{
		Use:   "environments",
		Short: "List the environments declared in the config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if len(a.environments) == 0 {
				a.log.Info("no environments declared; this config targets a single environment")
				return nil
			}
			for _, name := range a.environments {
				a.log.Info("%s", name)
			}
			return nil
		},
	}

	cmd.AddCommand(show, validate, envs)
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
