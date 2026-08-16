package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/danecwalker/buidl/internal/build"
	"github.com/danecwalker/buidl/internal/cluster"
	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/deploy/kubernetes"
	"github.com/danecwalker/buidl/internal/gitinfo"
	"github.com/danecwalker/buidl/internal/hooks"
	"github.com/danecwalker/buidl/internal/release"
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
		Use:    "build",
		Hidden: true,
		Short:  "Build and push a container image",
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

			// Only the commands that mint a release need a commit to attribute
			// it to. Lint and inspection commands work fine without one.
			if err := a.git.RequireCommit(); err != nil {
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
		skipBuild        bool
		noWait           bool
		autoRollback     bool
		noCache          bool
		releaseID        string
		digest           string
		allowDirty       bool
		yes              bool
		skipCluster      bool
		dryRun           bool
		detailed         bool
		detailedExitCode bool
	)

	cmd := &cobra.Command{
		Use:   "deploy [APP]",
		Short: "Converge the cluster if needed, then build and deploy a release",
		Long: `Build the image, push it, and roll it out.

With no name this deploys every process app in the stack and creates
stateful apps (postgres, redis) that are not in the cluster yet. Later
deploys leave existing stateful apps alone. ` + "`buidl deploy postgres`" + `
reconciles that one (and can restart it).

` + "`--dry-run`" + ` prints the plan and changes nothing.

When an ` + "`infra`" + ` block is present, the cluster is brought to its configured
state first. A fresh set of servers gets Kubernetes installed and joined; an
existing cluster is left alone.

Examples:
  buidl deploy
  buidl deploy --dry-run
  buidl deploy api
  buidl deploy postgres --yes
  buidl deploy -e production --auto-rollback
  buidl deploy -e production --skip-build --digest sha256:abc...`,
		Args: cobra.MaximumNArgs(1),
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

			targetApp := ""
			if len(args) == 1 {
				targetApp = args[0]
			}
			if dryRun {
				return a.runPlan(cmd, targetApp, detailed, detailedExitCode, digest)
			}

			// Keep the stack config on a.cfg. Extra process clones change
			// Config.App (object names); the kubeconfig context and
			// accessory list stay on the stack.
			stack := a.cfg
			processCfg := stack
			if targetApp != "" {
				switch stack.Member(targetApp) {
				case config.MemberNone:
					return stack.UnknownAppError(targetApp)
				case config.MemberStateful:
					return a.reconcileStateful(cmd, targetApp, yes)
				case config.MemberProcess, config.MemberPrimary:
					one, err := stack.ForProcessApp(targetApp)
					if err != nil {
						return err
					}
					processCfg = one
				}
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

						// The cluster being up to date says nothing about the
						// local kubeconfig: a CI runner starts with none, and
						// a previous run that failed after installing leaves
						// the cluster healthy with nothing fetched.
						if err := a.adoptManagedContext(cmd, mgr); err != nil {
							return err
						}
					}
				}
			}

			secretValues, err := a.resolveSecrets()
			if err != nil {
				return err
			}

			if err := a.git.RequireCommit(); err != nil {
				return err
			}

			rel := a.newRelease(releaseID)
			rel.Repo = processCfg.Image
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
				rel, err = a.buildReleaseFor(ctx, processCfg, rel, true, noCache, nil)
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
			req.Config = processCfg

			a.log.Step("Preflight checks")
			if err := target.Preflight(ctx, req); err != nil {
				return err
			}

			// Create accessories that are not in the cluster yet. Existing ones
			// are left alone so a later deploy cannot restart a database.
			// A named process deploy (web, api) skips this; a named
			// stateful deploy already returned above.
			if targetApp == "" {
				accReq := req
				accReq.Config = stack
				if err := ensureMissingAccessories(ctx, target, accReq); err != nil {
					return err
				}
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

			if targetApp == "" {
				if err := a.deployExtraProcesses(ctx, target, stack, rel, secretValues, !noWait, autoRollback, skipBuild, digest, noCache); err != nil {
					return err
				}
			}
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
	f.BoolVar(&dryRun, "dry-run", false, "print the plan and change nothing")
	f.BoolVar(&detailed, "detailed", false, "show full object diffs (with --dry-run)")
	f.BoolVar(&detailedExitCode, "detailed-exitcode", false, "exit 2 when --dry-run detects changes")

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
	return config.ProductionLike(env)
}

// newPlanCmd is the hidden alias of `buidl deploy --dry-run`.
func newPlanCmd(a *App) *cobra.Command {
	var (
		detailed         bool
		detailedExitCode bool
		digest           string
	)

	cmd := &cobra.Command{
		Use:    "plan [APP]",
		Hidden: true,
		Short:  "Show what a deploy would change (alias of deploy --dry-run)",
		Long: `Hidden alias of ` + "`buidl deploy --dry-run`" + `. Dry-run a deploy and print
the resulting changes. Nothing is changed.

When an ` + "`infra`" + ` block is present, the servers are inspected first and any
Kubernetes installation they need is part of the plan.

The application diff is computed by the Kubernetes API server, so it reflects the
same defaulting and admission logic a real apply would go through.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			targetApp := ""
			if len(args) == 1 {
				targetApp = args[0]
			}
			return a.runPlan(cmd, targetApp, detailed, detailedExitCode, digest)
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
		Use:    "promote",
		Hidden: true,
		Short:  "Deploy the image running in one environment to another",
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
		Use:   "rollback [APP]",
		Short: "Revert to a previous release",
		Long: `Roll back to the previous release, or to a specific one.

Rollback reuses the exact pod template from a prior revision, so it neither
rebuilds nor re-resolves any tag. Run ` + "`buidl status --history`" + ` to see what is available.

With no name this rolls back every process app. ` + "`buidl rollback api`" + `
rolls back only that app.

Examples:
  buidl rollback
  buidl rollback api
  buidl rollback -e production --to a1b2c3d-3lk2j9`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if err := a.confirmProduction(cmd, yes); err != nil {
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

			names := []string{a.cfg.App}
			if len(args) == 1 {
				cfg, err := a.processAppOrError(args[0])
				if err != nil {
					return err
				}
				names = []string{cfg.App}
			} else {
				names = a.cfg.ProcessAppNames()
			}

			var last *deploy.Outcome
			for _, name := range names {
				cfg, err := a.cfg.ForProcessApp(name)
				if err != nil {
					return err
				}
				a.log.Step(fmt.Sprintf("Rolling back %s in %s", name, a.cfg.Environment))
				outcome, err := target.Rollback(ctx, deploy.RollbackRequest{
					Config: cfg,
					Root:   a.root,
					To:     to,
					Wait:   !noWait,
				})
				if err != nil {
					if len(names) > 1 {
						a.log.Warn("%s: %v", name, err)
						continue
					}
					return err
				}
				last = outcome
				a.log.EndStep()
				a.log.Success("rolled back %s to %s in %s", name, outcome.Release.ID, outcome.Duration.Round(time.Second))
				if outcome.URL != "" {
					a.log.Info("%s", outcome.URL)
				}
			}
			if last == nil {
				return fmt.Errorf("nothing to roll back in %s", a.cfg.Environment)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&to, "to", "", "release id or revision number to roll back to (default: the previous release)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the rollout")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the production confirmation prompt")

	return cmd
}

// ensureMissingAccessories creates accessory objects that are not in the
// cluster. Other backends have no accessories; a type-assert miss is a no-op.
func ensureMissingAccessories(ctx context.Context, target deploy.Target, req deploy.Request) error {
	kt, ok := target.(*kubernetes.Target)
	if !ok {
		return nil
	}
	return kt.EnsureMissingAccessories(ctx, req)
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

// processAppOrError resolves a process app by name. Stateful names are
// rejected because status / logs / rollback talk to Deployments, not
// StatefulSets.
func (a *App) processAppOrError(name string) (*config.Config, error) {
	switch a.cfg.Member(name) {
	case config.MemberNone:
		return nil, a.cfg.UnknownAppError(name)
	case config.MemberStateful:
		return nil, fmt.Errorf("%q is a stateful app; reconcile it with `buidl deploy %s`", name, name)
	default:
		return a.cfg.ForProcessApp(name)
	}
}

// buildReleaseFor builds using cfg's image repository without changing the
// stack config on a.cfg (cluster context is named after the stack app).
func (a *App) buildReleaseFor(ctx context.Context, cfg *config.Config, rel release.Release, push, noCache bool, platforms []string) (release.Release, error) {
	prev := a.cfg
	a.cfg = cfg
	defer func() { a.cfg = prev }()
	return a.buildRelease(ctx, rel, push, noCache, platforms)
}

// deployExtraProcesses rolls out every extra process app after the first
// process. Same image as the primary reuses that digest; a different
// repository is built (or resolved) on its own.
func (a *App) deployExtraProcesses(ctx context.Context, target deploy.Target, stack *config.Config, primary release.Release, secretValues map[string]string, wait, autoRollback, skipBuild bool, digest string, noCache bool) error {
	extras := stack.ProcessAppNames()
	if len(extras) <= 1 {
		return nil
	}
	for _, name := range extras[1:] {
		cfg, err := stack.ForProcessApp(name)
		if err != nil {
			return err
		}
		rel := a.newRelease(primary.ID)
		rel.Repo = cfg.Image
		rel.Git = primary.Git
		switch {
		case cfg.Image == primary.Repo && primary.Digest != "":
			rel.Digest = primary.Digest
		case skipBuild:
			a.log.Step(fmt.Sprintf("Resolving %s", rel.TagRef()))
			resolved, err := build.Resolve(ctx, rel.TagRef())
			if err != nil {
				return fmt.Errorf("%s: %w\n\nhint: run `buidl build` first, or drop --skip-build", name, err)
			}
			rel.Digest = resolved
		default:
			rel, err = a.buildReleaseFor(ctx, cfg, rel, true, noCache, nil)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}

		secrets := secretValues
		if extraSecrets, err := a.resolveSecretsFor(cfg); err == nil {
			secrets = extraSecrets
		} else {
			return fmt.Errorf("%s: %w", name, err)
		}

		req := a.deployRequest(rel, secrets, wait, autoRollback)
		req.Config = cfg
		a.log.Step(fmt.Sprintf("Preflight checks (%s)", name))
		if err := target.Preflight(ctx, req); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		outcome, err := target.Deploy(ctx, req)
		if err != nil {
			a.log.FailStep(err)
			a.reportPartialFailure(outcome)
			return fmt.Errorf("%s: %w", name, err)
		}
		a.reportOutcome(outcome)
	}
	return nil
}

// resolveSecretsFor loads secrets against cfg without leaving a.cfg swapped.
func (a *App) resolveSecretsFor(cfg *config.Config) (map[string]string, error) {
	prev := a.cfg
	a.cfg = cfg
	defer func() { a.cfg = prev }()
	return a.resolveSecrets()
}

// runPlan is `deploy --dry-run` and the hidden `plan` command.
func (a *App) runPlan(cmd *cobra.Command, targetApp string, detailed, detailedExitCode bool, digest string) error {
	ctx := cmd.Context()
	stack := a.cfg
	if targetApp != "" && stack.Member(targetApp) == config.MemberNone {
		return stack.UnknownAppError(targetApp)
	}

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

		if !clusterChangesPending {
			if err := a.adoptManagedContext(cmd, mgr); err != nil {
				return err
			}
		}
		a.log.Info("")
	}

	if stack.Member(targetApp) == config.MemberStateful {
		return a.planStateful(cmd, targetApp, detailed, detailedExitCode, clusterChangesPending || addonsPending)
	}

	secretValues, err := a.resolveSecrets()
	if err != nil {
		return err
	}

	target, err := a.target()
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
	defer target.Close()

	names := stack.ProcessAppNames()
	if targetApp != "" {
		names = []string{targetApp}
	}

	resolved := map[string]string{}
	anyChanges := clusterChangesPending || addonsPending
	for _, name := range names {
		proc, err := stack.ForProcessApp(name)
		if err != nil {
			return err
		}
		rel := a.newRelease("")
		rel.Repo = proc.Image
		switch {
		case digest != "":
			rel.Digest = digest
		default:
			if d, ok := resolved[proc.Image]; ok {
				rel.Digest = d
			} else if got, err := build.Resolve(ctx, rel.TagRef()); err == nil {
				rel.Digest = got
				resolved[proc.Image] = got
			} else {
				rel.Digest = placeholderDigest
				resolved[proc.Image] = placeholderDigest
				a.log.Warn("no image found for %s; planning with a placeholder digest", rel.TagRef())
			}
		}

		a.log.Step(fmt.Sprintf("Planning %s -> %s", name, a.cfg.Environment))
		req := a.deployRequest(rel, secretValues, false, false)
		req.Config = proc
		plan, err := target.Plan(ctx, req)
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
		if plan.HasChanges() {
			anyChanges = true
		}
	}

	if targetApp == "" && len(stack.Accessories) > 0 {
		if err := a.planMissingAccessories(cmd, target, detailed, &anyChanges); err != nil {
			return err
		}
	}

	if detailedExitCode && anyChanges {
		return &exitCodeError{code: ExitChangesFound, err: fmt.Errorf("changes detected")}
	}
	return nil
}

// planMissingAccessories shows only creates: a stack dry-run must not
// imply that deploy would update an existing database.
func (a *App) planMissingAccessories(cmd *cobra.Command, target deploy.Target, detailed bool, anyChanges *bool) error {
	kt, ok := target.(*kubernetes.Target)
	if !ok {
		return nil
	}
	secretValues, err := a.resolveSecrets()
	if err != nil {
		return err
	}
	req := a.deployRequest(a.newRelease(""), secretValues, false, false)
	plan, err := kt.PlanAccessories(cmd.Context(), req)
	if err != nil {
		return err
	}
	var creates []deploy.Change
	for _, c := range plan.Changes {
		if c.Action == deploy.ActionCreate {
			creates = append(creates, c)
		}
	}
	if len(creates) == 0 {
		a.log.Detail("stateful apps already present; left alone")
		return nil
	}
	plan.Changes = creates
	a.renderAccessoryPlan(plan, detailed)
	*anyChanges = true
	return nil
}

// planStateful dry-runs one typed accessory (deploy --dry-run postgres).
func (a *App) planStateful(cmd *cobra.Command, name string, detailed, detailedExitCode, clusterPending bool) error {
	filtered, err := a.cfg.ForStatefulApp(name)
	if err != nil {
		return err
	}
	prev := a.cfg
	a.cfg = filtered
	defer func() { a.cfg = prev }()

	target, req, err := a.accessoryRequest(cmd)
	if err != nil {
		return err
	}
	defer target.Close()

	plan, err := target.PlanAccessories(cmd.Context(), req)
	if err != nil {
		return err
	}
	a.renderAccessoryPlan(plan, detailed)
	if detailedExitCode && (plan.HasChanges() || clusterPending) {
		return &exitCodeError{code: ExitChangesFound, err: fmt.Errorf("changes detected")}
	}
	return nil
}

// reconcileStateful is `buidl deploy postgres`: the old accessory apply.
func (a *App) reconcileStateful(cmd *cobra.Command, name string, yes bool) error {
	filtered, err := a.cfg.ForStatefulApp(name)
	if err != nil {
		return err
	}
	prev := a.cfg
	a.cfg = filtered
	defer func() { a.cfg = prev }()

	target, req, err := a.accessoryRequest(cmd)
	if err != nil {
		return err
	}
	defer target.Close()

	plan, err := target.PlanAccessories(cmd.Context(), req)
	if err != nil {
		return err
	}
	a.renderAccessoryPlan(plan, false)

	if err := a.confirmAccessoryApply(cmd, plan, yes); err != nil {
		return err
	}

	changes, err := target.ApplyAccessories(cmd.Context(), req)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		a.log.Success("no changes for %s", name)
		return nil
	}

	a.log.EndStep()
	a.log.Success("applied %d object(s) for %s", len(changes), name)
	a.log.Detail("watch it come up with `kubectl rollout status statefulset -n %s -l app.kubernetes.io/component=accessory`",
		a.cfg.Deploy.Kubernetes.Namespace)
	return nil
}
