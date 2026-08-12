package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/danewalker/buidl/internal/build"
	"github.com/danewalker/buidl/internal/cluster"
	"github.com/danewalker/buidl/internal/deploy"
	"github.com/danewalker/buidl/internal/gitinfo"
	"github.com/danewalker/buidl/internal/hooks"
)

// newBuildCmd builds and pushes an image without deploying.
//
// Splitting build from deploy is what lets a pipeline build once and deploy the
// same digest to several environments.
func newBuildCmd(a *App) *cobra.Command {
	var (
		push      bool
		noCache   bool
		platforms []string
		releaseID string
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build and push a container image",
		Long: `Build the application image with BuildKit and push it to the registry.

No Docker daemon is required. The image is exported straight to the registry and
identified by its digest, which is printed on success so a later deploy can pin
to it exactly.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if err := a.requireConfig(ctx); err != nil {
				return err
			}

			rel := a.newRelease(releaseID)
			rel, err := a.buildRelease(ctx, rel, push, noCache, platforms)
			if err != nil {
				return err
			}

			a.log.EndStep()
			a.log.Fields("build complete", map[string]any{
				"release": rel.ID,
				"digest":  rel.Digest,
				"ref":     rel.Ref(),
			})
			if !push {
				a.log.Warn("image was not pushed (--push=false); it cannot be deployed")
			}

			a.exportToCI(map[string]string{
				"release": rel.ID,
				"digest":  rel.Digest,
				"image":   rel.Ref(),
			})
			return nil
		},
	}

	cmd.Flags().BoolVar(&push, "push", true, "push the image to the registry")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "ignore the build cache")
	cmd.Flags().StringSliceVar(&platforms, "platform", nil, "target platforms (e.g. linux/amd64,linux/arm64)")
	cmd.Flags().StringVar(&releaseID, "release", "", "override the generated release id")

	return cmd
}

// newDeployCmd is the main path: build, push, apply, wait.
func newDeployCmd(a *App) *cobra.Command {
	var (
		skipBuild    bool
		noWait       bool
		autoRollback bool
		noCache      bool
		releaseID    string
		digest       string
		allowDirty   bool
		yes          bool
		skipCluster  bool
	)

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Converge the cluster if needed, then build and deploy a release",
		Long: `Build the image, push it, and roll it out.

When an ` + "`infra`" + ` block is present, the cluster is brought to its configured
state first. A fresh set of servers gets Kubernetes installed and joined; an
existing cluster is left alone. There is no separate bootstrap command — the plan
knows the difference, so the same command works either way.

The rollout is gated on health checks: deploy only succeeds once the new release
is actually serving. That makes it safe to use as a CI gate.

Examples:
  buidl deploy -e staging
  buidl deploy -e production --auto-rollback
  buidl deploy -e production --skip-cluster
  buidl deploy -e production --skip-build --digest sha256:abc...`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if err := a.requireEnvironment(); err != nil {
				return err
			}

			// Deploying uncommitted work produces a release nobody can reproduce.
			// Allowed locally with a warning; blocked in CI, where it signals a
			// broken pipeline rather than a deliberate choice.
			if a.git.Dirty && !allowDirty {
				if a.log.CI().Detected {
					return fmt.Errorf("refusing to deploy a dirty working tree in CI\n\nhint: commit your changes, or pass --allow-dirty if this is intentional")
				}
				a.log.Warn("deploying uncommitted changes; this release is not reproducible from a commit")
			}

			if err := a.confirmProduction(cmd, yes); err != nil {
				return err
			}

			// Converge the cluster before anything that needs to talk to it.
			// Building first would waste a long build on a cluster that turns out
			// not to exist.
			if !skipCluster {
				mgr, clusterPlan, err := a.clusterPlan(ctx)
				if err != nil {
					return err
				}
				if mgr != nil {
					defer mgr.Close()

					if !clusterPlan.Actionable() {
						a.renderClusterPlan(clusterPlan, false)
						return errClusterUnknown()
					}

					if clusterPlan.HasChanges() {
						a.renderClusterPlan(clusterPlan, false)
						if err := a.convergeCluster(cmd, mgr, clusterPlan, yes); err != nil {
							return err
						}
					} else {
						a.log.Detail("cluster is up to date (%s)", clusterPlan.Inventory.Summary())

						// Addons are verified on every deploy, not only when the
						// cluster is first built. Otherwise enabling one on an
						// existing cluster silently does nothing, and the symptom is
						// remote from the cause: proxy.ssl renders an Ingress
						// annotated for an issuer that was never installed, and the
						// site quietly serves the ingress controller's self-signed
						// certificate. Installed addons are detected and skipped.
						if err := mgr.ApplyAddons(ctx); err != nil {
							return err
						}

						// Point the deploy at the managed cluster — but only after
						// confirming this machine actually has its credentials. The
						// cluster being up to date says nothing about the local
						// kubeconfig: a CI runner starts with none, and a previous run
						// that failed after installing leaves the cluster healthy with
						// nothing fetched. Assuming the context exists turns that into
						// "context not found" against a perfectly good cluster.
						if a.cfg.Deploy.Kubernetes.Context == "" {
							contextName := a.defaultContextName()
							if !cluster.ContextExists(contextName) {
								a.log.Detail("no local credentials for %s; fetching", contextName)
								if err := a.fetchKubeconfig(cmd, mgr, contextName, "", false); err != nil {
									return fmt.Errorf("the cluster is up to date but its kubeconfig could not be fetched: %w", err)
								}
							}
							a.cfg.Deploy.Kubernetes.Context = contextName
						}
					}
				}
			}

			secretValues, err := a.resolveSecrets()
			if err != nil {
				return err
			}

			rel := a.newRelease(releaseID)
			hookCtx := a.hookContext(rel, secretValues, "")

			if err := a.runHook(ctx, hooks.PreBuild, hookCtx); err != nil {
				return err
			}

			switch {
			case digest != "":
				// An explicit digest deploys a specific, already-built artifact.
				rel.Digest = digest
			case skipBuild:
				// Resolve the tag that a previous `buidl build` pushed.
				a.log.Step("Resolving image")
				resolved, err := build.Resolve(ctx, rel.TagRef())
				if err != nil {
					return fmt.Errorf("%w\n\nhint: run `buidl build` first, or drop --skip-build", err)
				}
				rel.Digest = resolved
				a.log.Detail("resolved %s", rel.ShortDigest())
			default:
				rel, err = a.buildRelease(ctx, rel, true, noCache, nil)
				if err != nil {
					return err
				}
			}

			// The digest is only known after the build, so refresh the hook context.
			hookCtx = a.hookContext(rel, secretValues, "")
			if err := a.runHook(ctx, hooks.PostBuild, hookCtx); err != nil {
				return err
			}

			target, err := a.target()
			if err != nil {
				return err
			}
			defer target.Close()

			req := a.deployRequest(rel, secretValues, !noWait, autoRollback)

			a.log.Step("Preflight checks")
			if err := target.Preflight(ctx, req); err != nil {
				return err
			}

			// Migrations belong here: the image exists and the cluster is reachable,
			// but nothing is serving the new release yet. A failure aborts before
			// any change is applied.
			if err := a.runHook(ctx, hooks.PreDeploy, hookCtx); err != nil {
				a.log.FailStep(err)
				a.runFailureHook(ctx, hookCtx)
				a.log.Summary("Deploy summary")
				return err
			}

			outcome, err := target.Deploy(ctx, req)
			if err != nil {
				// Report what landed before failing, so the user knows the cluster's
				// actual state rather than only that something went wrong.
				a.log.FailStep(err)
				a.reportPartialFailure(outcome)
				a.runFailureHook(ctx, hookCtx)
				a.log.Summary("Deploy summary")
				return err
			}

			hookCtx.URL = outcome.URL
			if err := a.runHook(ctx, hooks.PostDeploy, hookCtx); err != nil {
				return err
			}

			a.reportOutcome(outcome)
			return nil
		},
	}

	f := cmd.Flags()
	f.BoolVar(&skipBuild, "skip-build", false, "deploy an image already in the registry")
	f.BoolVar(&noWait, "no-wait", false, "return without waiting for the rollout to become healthy")
	f.BoolVar(&autoRollback, "auto-rollback", false, "revert automatically if the rollout fails")
	f.BoolVar(&noCache, "no-cache", false, "ignore the build cache")
	f.StringVar(&releaseID, "release", "", "override the generated release id")
	f.StringVar(&digest, "digest", "", "deploy this exact image digest")
	f.BoolVar(&allowDirty, "allow-dirty", false, "allow deploying uncommitted changes")
	f.BoolVarP(&yes, "yes", "y", false, "skip confirmation prompts")
	f.BoolVar(&skipCluster, "skip-cluster", false, "do not inspect or change the cluster described by infra")

	return cmd
}

// confirmProduction guards interactive deploys to a production-like environment.
//
// Only prompts on a terminal: in CI there is nobody to answer, and the pipeline
// itself is the approval gate.
func (a *App) confirmProduction(cmd *cobra.Command, yes bool) error {
	if yes || a.log.CI().Detected || a.log.Mode() != "pretty" {
		return nil
	}
	if !isProductionLike(a.cfg.Environment) {
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "About to deploy %s to %s (cluster: %s).\n",
		a.cfg.App, a.cfg.Environment, a.clusterDescription())
	return a.confirm(cmd, "Continue?", "deploy cancelled")
}

// clusterDescription names the cluster a command is about to change.
//
// The confirmation prompt exists to answer one question — which cluster is this
// about to touch — and the config's context field is still empty when it runs on
// a deploy, because convergence sets that later. Printing it directly produced
// "(cluster: )" from the one prompt whose entire job is to say. So fall back to
// the context buidl manages for this environment, and then to whatever the
// kubeconfig currently selects, which is what the deploy would in fact use.
func (a *App) clusterDescription() string {
	if name := a.cfg.Deploy.Kubernetes.Context; name != "" {
		return name
	}
	if a.cfg.Infra != nil {
		return a.defaultContextName()
	}
	if current := cluster.CurrentContext(); current != "" {
		return current + " (current kubeconfig context)"
	}
	return "unknown"
}

// isProductionLike recognizes the environment names that warrant a confirmation.
func isProductionLike(env string) bool {
	switch env {
	case "production", "prod", "live", "main":
		return true
	}
	return false
}

// newPlanCmd shows what a deploy would change.
func newPlanCmd(a *App) *cobra.Command {
	var (
		detailed         bool
		detailedExitCode bool
		digest           string
	)

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what a deploy would change, including the cluster itself",
		Long: `Dry-run a deploy and print the resulting changes. Nothing is changed.

When an ` + "`infra`" + ` block is present, the servers are inspected first and any
Kubernetes installation they need is part of the plan. That makes this the single
place to see everything a deploy would do — bringing up a fresh fleet, joining a
new worker, upgrading a pinned version, and the application rollout itself.

The application diff is computed by the Kubernetes API server, so it reflects the
same defaulting and admission logic a real apply would go through. If the cluster
does not exist yet there is nothing to diff against, and the plan reports the
cluster work alone.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}

			// Cluster first: if it does not exist, the app plan cannot be computed
			// and saying so is more useful than a connection error.
			clusterChangesPending := false
			addonsPending := false
			if mgr, clusterPlan, err := a.clusterPlan(ctx); err != nil {
				return err
			} else if mgr != nil {
				defer mgr.Close()
				a.renderClusterPlan(clusterPlan, detailed)

				if !clusterPlan.Actionable() {
					return errClusterUnknown()
				}
				clusterChangesPending = clusterPlan.HasChanges()
				// Tracked apart from the server changes above: a missing addon does
				// not mean the cluster is absent, so it must not trigger the "the
				// application plan will be available once the cluster exists"
				// fallbacks — but it is still a change a deploy would make, so it
				// belongs in the exit code.
				addonsPending = len(clusterPlan.PendingAddons()) > 0

				// Only target the managed cluster once we know this machine holds
				// its credentials. Writing the field unconditionally would pin a
				// context that does not exist locally, hiding the missing-credentials
				// error target() raises — the one that names the command to fix it.
				if !clusterChangesPending && a.cfg.Deploy.Kubernetes.Context == "" {
					if name := a.defaultContextName(); cluster.ContextExists(name) {
						a.cfg.Deploy.Kubernetes.Context = name
					}
				}
				a.log.Info("")
			}

			secretValues, err := a.resolveSecrets()
			if err != nil {
				return err
			}

			rel := a.newRelease("")
			switch {
			case digest != "":
				rel.Digest = digest
			default:
				// Planning must not build. Resolve whatever is in the registry for
				// this release tag; if that is absent, fall back to a placeholder so
				// the rest of the plan is still useful.
				if resolved, err := build.Resolve(ctx, rel.TagRef()); err == nil {
					rel.Digest = resolved
				} else {
					rel.Digest = placeholderDigest
					a.log.Warn("no image found for %s; planning with a placeholder digest", rel.TagRef())
				}
			}

			target, err := a.target()
			if err != nil {
				// A missing cluster is expected when the cluster plan above has not
				// been applied yet, and is not a planning failure.
				if clusterChangesPending {
					a.log.Info("the application plan will be available once the cluster exists")
					a.log.Info("run `buidl deploy` to converge the cluster and roll out the app")
					if detailedExitCode {
						return &exitCodeError{code: ExitChangesFound, err: fmt.Errorf("changes detected")}
					}
					return nil
				}
				return err
			}
			defer target.Close()

			a.log.Step(fmt.Sprintf("Planning %s -> %s", a.cfg.App, a.cfg.Environment))
			plan, err := target.Plan(ctx, a.deployRequest(rel, secretValues, false, false))
			if err != nil {
				if clusterChangesPending {
					a.log.Info("the application plan will be available once the cluster exists")
					a.log.Info("run `buidl deploy` to converge the cluster and roll out the app")
					if detailedExitCode {
						return &exitCodeError{code: ExitChangesFound, err: fmt.Errorf("changes detected")}
					}
					return nil
				}
				return err
			}

			a.log.EndStep()
			a.renderPlan(plan, detailed)

			// Terraform-style: exit 2 signals "there are changes", which lets a
			// pipeline require an approval step only when something would change.
			// Cluster changes count too — they are part of what a deploy would do.
			if detailedExitCode && (plan.HasChanges() || clusterChangesPending || addonsPending) {
				return &exitCodeError{code: ExitChangesFound, err: fmt.Errorf("changes detected")}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&detailed, "detailed", false, "show full object diffs")
	cmd.Flags().BoolVar(&detailedExitCode, "detailed-exitcode", false, "exit 2 when changes are detected")
	cmd.Flags().StringVar(&digest, "digest", "", "plan against this image digest")

	return cmd
}

// placeholderDigest lets `plan` render a complete manifest before any image
// exists. It is never deployed: Render requires a real digest, and this value is
// only substituted in plan mode.
const placeholderDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// newPromoteCmd deploys one environment's exact image to another.
func newPromoteCmd(a *App) *cobra.Command {
	var (
		from   string
		to     string
		noWait bool
		yes    bool
	)

	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Deploy the image running in one environment to another",
		Long: `Promote the exact image digest currently live in one environment to another.

Nothing is rebuilt. The bytes that were tested in staging are the bytes that run
in production, which is the entire point: a rebuild could pick up a new base
image layer or a different dependency resolution and quietly differ from what you
verified.

Example:
  buidl promote --from staging --to production`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if from == "" || to == "" {
				return fmt.Errorf("both --from and --to are required")
			}
			if from == to {
				return fmt.Errorf("--from and --to must differ")
			}
			// -e is overwritten below by --from and then --to, so accepting it would
			// let `promote -e staging --from a --to b` read as though it had anything
			// to do with staging.
			if a.opts.environment != "" {
				return fmt.Errorf("`promote` selects environments with --from and --to; drop -e/--env")
			}

			// Read the source environment's live state.
			a.opts.environment = from
			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			// Without this, promote reads the source's state from whatever
			// kubeconfig context happens to be current — a different cluster on a
			// CI runner or a colleague's machine. Reading the wrong cluster is
			// worse than failing, because the digest it returns looks plausible.
			if err := a.ensureClusterCredentials(cmd); err != nil {
				return err
			}

			sourceTarget, err := a.target()
			if err != nil {
				return err
			}
			a.log.Step(fmt.Sprintf("Reading %s", from))
			status, err := sourceTarget.Status(ctx, deploy.Request{Config: a.cfg, Root: a.root})
			sourceTarget.Close()
			if err != nil {
				return fmt.Errorf("cannot read %s: %w", from, err)
			}
			if status.Digest == "" {
				return fmt.Errorf("%s has no recorded image digest; redeploy it with this version of buidl first", from)
			}
			a.log.Info("%s is running %s (%s)", from, status.Release, shortDigest(status.Digest))
			sourceImage := a.cfg.Image

			// Reload config for the destination environment.
			a.cfg = nil
			a.opts.environment = to
			if err := a.requireConfig(ctx); err != nil {
				return err
			}

			if err := checkPromoteRepositories(from, sourceImage, to, a.cfg.Image); err != nil {
				return err
			}

			if err := a.ensureClusterCredentials(cmd); err != nil {
				return err
			}

			if err := a.confirmProduction(cmd, yes); err != nil {
				return err
			}

			secretValues, err := a.resolveSecrets()
			if err != nil {
				return err
			}

			// Keep the source release ID so the same artifact is traceable across
			// environments, and pin the digest.
			rel := a.newRelease(status.Release)
			rel.Digest = status.Digest

			// Carry the source release's provenance rather than the local working
			// tree's. A promotion ships an artifact built from some other commit,
			// possibly days ago; annotating it with whatever is checked out here
			// would make `status` and `releases` report a commit that has nothing
			// to do with the running image — and a later rollback would restore
			// those wrong annotations, making the error permanent.
			rel.Git = gitinfo.Info{
				Available: status.GitSHA != "",
				SHA:       status.GitSHA,
			}

			target, err := a.target()
			if err != nil {
				return err
			}
			defer target.Close()

			req := a.deployRequest(rel, secretValues, !noWait, true)
			hookCtx := a.hookContext(rel, secretValues, "")

			a.log.Step("Preflight checks")
			if err := target.Preflight(ctx, req); err != nil {
				return err
			}

			// Migrations belong here just as much as on a deploy: promote is the
			// staging-to-production path, which is exactly where a schema change
			// must land before the new code serves.
			if err := a.runHook(ctx, hooks.PreDeploy, hookCtx); err != nil {
				a.log.FailStep(err)
				a.runFailureHook(ctx, hookCtx)
				return err
			}

			a.log.Step(fmt.Sprintf("Promoting %s -> %s", from, to))
			outcome, err := target.Deploy(ctx, req)
			if err != nil {
				a.log.FailStep(err)
				a.reportPartialFailure(outcome)
				a.runFailureHook(ctx, hookCtx)
				return err
			}

			hookCtx.URL = outcome.URL
			if err := a.runHook(ctx, hooks.PostDeploy, hookCtx); err != nil {
				return err
			}

			a.reportOutcome(outcome)
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "source environment")
	cmd.Flags().StringVar(&to, "to", "", "destination environment")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the rollout")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the production confirmation prompt")

	return cmd
}

// checkPromoteRepositories refuses a promotion whose digest and repository come
// from different environments.
//
// A digest identifies bytes within one repository, so pairing the source's digest
// with a destination that overlays a different `image` names a reference that does
// not exist. Preflight does catch it, as "image ... is not available", which reads
// like a registry outage and sends the user looking in the wrong place.
func checkPromoteRepositories(from, sourceImage, to, destImage string) error {
	if sourceImage == destImage {
		return nil
	}
	return fmt.Errorf("promote cannot cross repositories: %s builds to %s but %s builds to %s\n\n"+
		"hint: a digest exists only in the repository it was pushed to, so %s's image has no counterpart in %s.\n"+
		"Give both environments the same `image`, or build for %s with `buidl deploy -e %s`.",
		from, sourceImage, to, destImage, from, destImage, to, to)
}

// newRollbackCmd reverts to a previous release.
func newRollbackCmd(a *App) *cobra.Command {
	var (
		to     string
		noWait bool
		yes    bool
	)

	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Revert to a previous release",
		Long: `Roll back to the previous release, or to a specific one.

Rollback reuses the exact pod template from a prior revision, so it neither
rebuilds nor re-resolves any tag. Run ` + "`buidl releases`" + ` to see what is available.

Examples:
  buidl rollback -e production
  buidl rollback -e production --to a1b2c3d-3lk2j9`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if err := a.confirmProduction(cmd, yes); err != nil {
				return err
			}

			target, err := a.target()
			if err != nil {
				return err
			}
			defer target.Close()

			a.log.Step(fmt.Sprintf("Rolling back %s", a.cfg.Environment))
			outcome, err := target.Rollback(ctx, deploy.RollbackRequest{
				Config: a.cfg,
				Root:   a.root,
				To:     to,
				Wait:   !noWait,
			})
			if err != nil {
				return err
			}

			a.log.EndStep()
			a.log.Success("rolled back to %s in %s", outcome.Release.ID, outcome.Duration.Round(time.Second))
			if outcome.URL != "" {
				a.log.Info("%s", outcome.URL)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&to, "to", "", "release id or revision number to roll back to (default: the previous release)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the rollout")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the production confirmation prompt")

	return cmd
}

// shortDigest abbreviates a digest for display.
func shortDigest(d string) string {
	const prefix = "sha256:"
	hex := d
	if len(hex) > len(prefix) && hex[:len(prefix)] == prefix {
		hex = hex[len(prefix):]
	}
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return prefix + hex
}
