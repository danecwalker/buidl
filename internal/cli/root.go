// Package cli wires the command surface.
//
// The command names deliberately echo tools users already know: `deploy`,
// `rollback`, `logs` and `status` behave the way Kamal's equivalents do, while
// `plan`, `promote` and `releases` bring the immutable-release model that makes
// staging-to-production promotion exact rather than approximate.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/danewalker/buidl/internal/cluster"
	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/deploy"
	_ "github.com/danewalker/buidl/internal/deploy/kubernetes" // register the kubernetes backend
	"github.com/danewalker/buidl/internal/gitinfo"
	"github.com/danewalker/buidl/internal/release"
	"github.com/danewalker/buidl/internal/secrets"
	"github.com/danewalker/buidl/internal/ui"
)

// Version is set at link time via -ldflags.
var Version = "dev"

// globalOptions are flags shared by every command.
type globalOptions struct {
	configPath  string
	environment string
	output      string
	verbose     bool
	noColor     bool
	// timeout bounds the whole command, so a hung deploy in CI cannot run until
	// the job's own limit kills it without explanation.
	timeout time.Duration
}

// App holds resolved state shared by command implementations.
type App struct {
	opts *globalOptions
	log  *ui.Printer

	// Config and its provenance, loaded lazily by requireConfig.
	cfg  *config.Config
	root string
	path string
	// environments lists every environment declared in the config file.
	environments []string

	git gitinfo.Info
	// gitLoaded guards against re-running git for every config load in commands
	// that resolve several environments.
	gitLoaded bool
}

// Execute builds the command tree and runs it. It returns the process exit code.
func Execute() int {
	opts := &globalOptions{}
	app := &App{opts: opts}

	root := &cobra.Command{
		Use:   "buidl",
		Short: "Build and deploy applications to Kubernetes, cloud, and bare metal",
		Long: `buidl builds container images without a Docker daemon and deploys them as
immutable, digest-pinned releases.

Every deploy is a release you can inspect, promote, and roll back:

  buidl init                        detect the project and write buidl.yaml
  buidl deploy -e staging           build, push, and roll out
  buidl plan -e production          show exactly what would change
  buidl promote --from staging --to production
                                    deploy staging's exact image to production
  buidl rollback -e production      revert to the previous release`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
		// Without a subcommand, help is the useful response.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return app.setup()
		},
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&opts.configPath, "config", "f", "", "path to buidl.yaml (default: search up from the current directory)")
	pf.StringVarP(&opts.environment, "env", "e", "", "environment to target (e.g. staging, production)")
	pf.StringVarP(&opts.output, "output", "o", "auto", "output format: auto, pretty, plain, json")
	pf.BoolVarP(&opts.verbose, "verbose", "v", false, "show detailed progress")
	pf.BoolVar(&opts.noColor, "no-color", false, "disable colored output")
	pf.DurationVar(&opts.timeout, "timeout", 30*time.Minute, "abort the command after this duration")

	root.AddCommand(
		newInitCmd(app),
		newBuildCmd(app),
		newDeployCmd(app),
		newPlanCmd(app),
		newPromoteCmd(app),
		newRollbackCmd(app),
		newStatusCmd(app),
		newReleasesCmd(app),
		newLogsCmd(app),
		newManifestCmd(app),
		newConfigCmd(app),
		newClusterCmd(app),
		newEnvCmd(app),
		newHooksCmd(app),
	)

	if err := root.Execute(); err != nil {
		// The printer may not exist yet if setup itself failed.
		if app.log != nil {
			app.log.Error("%s", err)
		} else {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
		}
		return exitCode(err)
	}
	if app.log != nil && app.log.Failed() {
		return 1
	}
	return 0
}

// exitCodeError lets commands request a specific exit status. CI pipelines
// branch on these, so they are part of the tool's contract.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

// Exit codes. 2 is reserved for "changes detected" so `buidl plan --detailed-exitcode`
// can gate a pipeline the way terraform does.
const (
	ExitFailure       = 1
	ExitChangesFound  = 2
	ExitConfigInvalid = 3
)

func exitCode(err error) int {
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	var cfgErrs config.Errors
	if errors.As(err, &cfgErrs) {
		return ExitConfigInvalid
	}
	return ExitFailure
}

// setup initializes output. Config loading is deferred so commands that do not
// need it (init, version) work in an empty directory.
func (a *App) setup() error {
	mode := ui.Mode(a.opts.output)
	switch mode {
	case ui.ModeAuto, ui.ModePretty, ui.ModePlain, ui.ModeJSON:
	default:
		return fmt.Errorf("invalid --output %q (want auto, pretty, plain, or json)", a.opts.output)
	}

	a.log = ui.New(ui.Options{
		Mode:    mode,
		Verbose: a.opts.verbose,
		NoColor: a.opts.noColor,
	})
	return nil
}

// requireConfig loads and resolves the config for the selected environment.
func (a *App) requireConfig(ctx context.Context) error {
	if a.cfg != nil {
		return nil
	}

	if err := a.ensureGit(ctx); err != nil {
		return err
	}

	res, err := config.Load(config.LoadOptions{
		Path:        a.opts.configPath,
		Environment: a.opts.environment,
		Vars:        a.interpolationVars(a.git),
		Strict:      true,
	})
	if err != nil {
		return err
	}

	a.cfg = res.Config
	a.root = res.Root
	a.path = res.Path
	a.environments = res.Environments

	for _, warning := range secrets.PermissionWarnings(a.root, a.cfg.Environment) {
		a.log.Warn("%s", warning)
	}

	a.log.Detail("config %s (environment %s)", res.Path, res.Config.Environment)
	return nil
}

// ensureGit loads repository provenance once per invocation.
//
// It must run before any config parse: ${BUIDL_SLUG} and ${BUIDL_SHA} in a
// config file resolve from it, so a command that parses config without this
// populated would fail on an unset variable.
func (a *App) ensureGit(ctx context.Context) error {
	if a.gitLoaded {
		return nil
	}
	gi, err := gitinfo.Load(ctx, ".")
	if err != nil {
		return err
	}
	a.git = gi
	a.gitLoaded = true
	return nil
}

// interpolationVars builds the variable context available to buidl.yaml.
//
// These are what make one config file serve many environments: a preview
// environment's hostname and namespace are derived from the branch, so a new
// branch needs no configuration at all.
func (a *App) interpolationVars(gi gitinfo.Info) map[string]string {
	ci := ui.DetectCI()

	vars := map[string]string{
		"BUIDL_VERSION": Version,
	}
	if gi.Available {
		vars["BUIDL_SHA"] = gi.SHA
		vars["BUIDL_SHORT_SHA"] = shortSHA(gi.SHA)
		vars["BUIDL_BRANCH"] = gi.Branch
		vars["BUIDL_SLUG"] = gitinfo.Slug(gi.Branch)
		vars["BUIDL_GIT_TAG"] = gi.Tag
	}
	if ci.PullRequest != "" {
		vars["BUIDL_PR"] = ci.PullRequest
		// A PR-numbered slug is stabler than a branch slug: it survives a branch
		// rename mid-review.
		vars["BUIDL_SLUG"] = "pr-" + ci.PullRequest
	}
	if ci.Detected {
		vars["BUIDL_CI"] = ci.Provider
	}
	return vars
}

// newRelease mints the release for this invocation.
func (a *App) newRelease(overrideID string) release.Release {
	ci := ui.DetectCI()

	actor := ci.Actor
	if actor == "" {
		actor = localActor()
	}

	rel := release.New(a.cfg.Environment, a.git, time.Now(), actor)
	rel.Repo = a.cfg.Image
	if overrideID != "" {
		rel.ID = overrideID
	}
	rel.Tag = rel.ID
	return rel
}

// localActor identifies who is deploying from a workstation.
func localActor() string {
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return "unknown"
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// target constructs the deploy backend for the loaded config.
//
// When buidl manages the cluster, every command addresses it by the context buidl
// created rather than whatever happens to be current in the kubeconfig. Without
// this only `deploy` resolved the right cluster — it sets the context as part of
// convergence — while `status`, `releases`, `rollback` and `logs` silently used
// the current context, which is at best a different cluster and at worst a stale
// entry for one that no longer exists.
//
// The context is only adopted if it exists locally. On a fresh fleet it will not
// yet, and `deploy` fetches it as part of bringing the cluster up.
func (a *App) target() (deploy.Target, error) {
	if a.cfg.Infra != nil && a.cfg.Deploy.Kubernetes.Context == "" {
		if name := a.defaultContextName(); cluster.ContextExists(name) {
			a.cfg.Deploy.Kubernetes.Context = name
			a.log.Detail("targeting managed cluster context %s", name)
		}
	}
	return deploy.For(a.cfg, a.log)
}

// context returns a command context that cancels on SIGINT/SIGTERM and after the
// configured timeout.
//
// Handling the signal rather than dying immediately matters: an interrupted
// deploy should stop waiting, not abandon a half-applied rollout silently.
func (a *App) context() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), a.opts.timeout)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	return ctx, func() {
		stop()
		cancel()
	}
}

// requireEnvironment produces a helpful error when an environment is needed but
// was not given.
func (a *App) requireEnvironment() error {
	if a.opts.environment != "" || a.cfg.Environment != "default" {
		return nil
	}
	if len(a.environments) == 0 {
		return nil
	}
	return fmt.Errorf("this config declares environments (%s); pass -e", strings.Join(a.environments, ", "))
}
