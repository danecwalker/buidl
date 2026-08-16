package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/hooks"
	"github.com/danecwalker/buidl/internal/project"
	"github.com/danecwalker/buidl/internal/secrets"
)

// newInitCmd scaffolds a project.
func newInitCmd(a *App) *cobra.Command {
	var (
		appName  string
		image    string
		registry string
		force    bool
		writeCI  bool
		noCI     bool
		staging  bool
		preview  bool
		noDocker bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Detect the project and scaffold buidl.yaml",
		Long: `Inspect the current directory and write a working configuration.

On a terminal this is a short setup wizard: detect the stack, write the
file, then ask whether you want GitHub Actions, a staging environment, and
review apps. The answers are written for you. You should not need to edit
` + "`buidl.yaml`" + `.

Non-interactive runs (scripts, CI) skip the questions. Pass ` + "`--ci`" + `,
` + "`--staging`" + `, and ` + "`--preview`" + ` to answer them on the command
line, or ` + "`--no-ci`" + ` to skip Actions.

Detection covers Go, Node, Python, Ruby, Rust and static sites. If there is no
Dockerfile, a multi-stage one is generated for the detected stack. Common
changes after init go through ` + "`buidl add server`" + `, ` + "`buidl add domain`" + `,
` + "`buidl add postgres`" + `, and ` + "`buidl variable`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// init is purely local filesystem work: no cluster, no registry, and
			// so no cancellable context to thread through.
			dir, err := os.Getwd()
			if err != nil {
				return err
			}

			// init must work before a config exists, so it does not call
			// requireConfig. It does check for an existing one.
			configPath := filepath.Join(dir, "buidl.yaml")
			if _, err := os.Stat(configPath); err == nil && !force {
				return fmt.Errorf("buidl.yaml already exists\n\nhint: pass --force to overwrite it")
			}

			a.log.Step("Detecting project")
			det, err := project.Detect(dir)
			if err != nil {
				return err
			}

			if appName != "" {
				det.Name = appName
			}
			a.log.Info("stack: %s", describeDetection(det))
			for _, note := range det.Notes {
				a.log.Info("note: %s", note)
			}

			resolvedImage, err := resolveImage(image, registry, det.Name)
			if err != nil {
				return err
			}

			// Ask before writing, like create-react-app: answers shape the
			// files, and Ctrl-C leaves the directory untouched.
			choice, err := a.resolveInitCI(cmd, writeCI, noCI, staging, preview)
			if err != nil {
				return err
			}

			// Dockerfile first: if this fails, no config is written that references
			// a file that does not exist.
			if !det.HasDockerfile && !noDocker {
				a.log.Step("Writing Dockerfile")
				content, err := project.GenerateDockerfile(det)
				if err != nil {
					a.log.Warn("%s", err)
				} else if err := writeFile(filepath.Join(dir, "Dockerfile"), content, force); err != nil {
					return err
				} else {
					a.log.Success("wrote Dockerfile")
				}
			}

			a.log.Step("Writing buidl.yaml")
			cfgYAML := renderConfig(det, resolvedImage)
			if err := writeFile(configPath, cfgYAML, force); err != nil {
				return err
			}
			a.log.Success("wrote buidl.yaml")

			// A gitignored secrets file, so nobody's first instinct is to put
			// credentials in buidl.yaml.
			if err := a.scaffoldSecrets(dir, det); err != nil {
				return err
			}

			if err := a.applyInitCI(dir, force, choice); err != nil {
				return err
			}

			// Validate what we just wrote, so init never leaves a broken config.
			if err := a.validateInitConfig(configPath); err != nil {
				return err
			}

			a.log.EndStep()
			a.log.Success("ready")
			a.log.Info("")
			a.log.Info("next steps:")
			a.log.Info("  1. buidl add server <ip> --email you@example.com")
			a.log.Info("  2. buidl add domain <hostname>     # optional; add again for api.example.com")
			a.log.Info("  3. buidl add postgres              # optional")
			if choice.Staging {
				a.log.Info("  4. buidl deploy                    # staging (the default)")
			} else {
				a.log.Info("  4. buidl deploy")
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&appName, "app", "", "application name (default: detected)")
	f.StringVar(&image, "image", "", "image repository (e.g. ghcr.io/acme/web)")
	f.StringVar(&registry, "registry", "", "registry host to build the image reference from (e.g. ghcr.io/acme)")
	f.BoolVar(&force, "force", false, "overwrite existing files")
	f.BoolVar(&writeCI, "ci", false, "set up GitHub Actions (skip the question)")
	f.BoolVar(&noCI, "no-ci", false, "do not set up GitHub Actions (skip the question)")
	f.BoolVar(&staging, "staging", false, "add a staging environment and a promote-to-production workflow")
	f.BoolVar(&preview, "preview", false, "add review apps (a preview environment per pull request)")
	f.BoolVar(&noDocker, "no-dockerfile", false, "skip generating a Dockerfile")

	return cmd
}

// describeDetection renders a one-line summary of what was detected.
func describeDetection(d project.Detection) string {
	parts := []string{string(d.Kind)}
	if d.Stack != "" && d.Stack != d.Kind {
		parts = append(parts, string(d.Stack))
	}
	if d.Framework != "" {
		parts = append(parts, d.Framework)
	}
	if d.Runtime != "" {
		parts = append(parts, d.Runtime)
	}
	if d.PackageManager != "" {
		parts = append(parts, string(d.PackageManager))
	}
	return strings.Join(parts, " / ")
}

// resolveImage determines the image repository to write into the config.
func resolveImage(image, registry, app string) (string, error) {
	if image != "" {
		return strings.ToLower(image), nil
	}
	if registry != "" {
		return strings.ToLower(strings.TrimSuffix(registry, "/") + "/" + app), nil
	}
	// A placeholder is better than a guess: the user must supply a real registry.
	// It must be lowercase, because an image reference with uppercase characters is
	// invalid — an uppercase placeholder would make the config buidl just wrote
	// fail its own validation.
	return "ghcr.io/change-me/" + app, nil
}

// renderConfig produces the starter buidl.yaml.
//
// Written as a template rather than by marshaling a struct so the file carries
// explanatory comments. A deploy config is read far more often than it is
// written, and the comments are most of its value.
func renderConfig(d project.Detection, image string) string {
	var b strings.Builder

	fmt.Fprintf(&b, `# buidl configuration. Docs: https://github.com/danecwalker/buidl
version: %d

app: %s
image: %s

registry:
  # The cluster cannot use your local docker login. Copy that credential
  # into an imagePullSecret so pods can pull the image you just pushed.
  # Set false for a public image, or set pullSecret to use one you manage.
  createPullSecret: true

build:
  # buildkit builds without a Docker daemon and pushes straight to the registry.
  driver: buildkit
  dockerfile: %s
  # Add linux/arm64 here to publish a multi-arch image.
  platforms: [linux/amd64]

deploy:
  target: kubernetes
  port: %d
  kubernetes:
    # Shown because the omitted default is on. A first deploy into a
    # new cluster has no app namespace. Set false to manage it yourself.
    createNamespace: true
  # Replica count is omitted on purpose. HTTP apps get a HorizontalPodAutoscaler
  # sized from the fleet (or the cluster's Ready nodes). Set replicas to pin a
  # static count, or set autoscale.min / autoscale.max to take over the bounds.

  healthcheck:
    # /startupz must pass before liveness or readiness run.
    # /readyz gates traffic and the rollout. Return 200 only when the
    # app can serve (dependencies included).
    # /livez restarts a wedged process. Keep it cheap — do not check
    # Postgres here, or a blip kills the pod.
    # Set path: /up to use one endpoint for all three (Rails/Kamal).
`, config.SchemaVersion, d.Name, image, dockerfilePath(d), d.Port)
	if d.HealthPath != "" {
		fmt.Fprintf(&b, "    path: %s\n", d.HealthPath)
	}
	fmt.Fprintf(&b, `
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {memory: 512Mi}

  strategy:
    # New pods come up first; traffic flips when they are healthy.
    type: bluegreen

env:
  clear:
    LOG_LEVEL: info
  # Names only — values are read from the environment or .buidl/secrets at
  # deploy time and are never written to this file.
  secret: []
`)

	return b.String()
}

func dockerfilePath(d project.Detection) string {
	if d.DockerfilePath != "" {
		return d.DockerfilePath
	}
	return "Dockerfile"
}

// scaffoldSecrets creates the .buidl directory: a committed declaration file, a
// gitignored values file, and sample hooks.
//
// The split mirrors Kamal's, because the distinction it encodes is genuinely
// useful. secrets-common is committed and declares which secrets exist and where
// they come from; the per-environment files are gitignored because people
// inevitably paste literal values into them.
func (a *App) scaffoldSecrets(dir string, d project.Detection) error {
	buidlDir := filepath.Join(dir, secrets.Directory)
	if err := os.MkdirAll(buidlDir, 0o755); err != nil {
		return err
	}

	// Committed: declares the bindings, holds no values.
	commonPath := filepath.Join(dir, secrets.CommonPath())
	if _, err := os.Stat(commonPath); os.IsNotExist(err) {
		if err := os.WriteFile(commonPath, []byte(secretsCommonTemplate(d)), 0o644); err != nil {
			return err
		}
		a.log.Success("wrote %s (safe to commit — declarations only)", secrets.CommonPath())
	}

	// Gitignored: may hold literal values.
	valuesPath := filepath.Join(dir, secrets.DefaultFile)
	if _, err := os.Stat(valuesPath); os.IsNotExist(err) {
		// 0600: this file holds credentials.
		if err := os.WriteFile(valuesPath, []byte(secretsValuesTemplate()), 0o600); err != nil {
			return err
		}
		a.log.Success("wrote %s (gitignored — put real values here)", secrets.DefaultFile)
	}

	if err := a.scaffoldHooks(dir); err != nil {
		return err
	}

	return a.ensureGitignore(dir)
}

// secretsCommonTemplate is the committed declaration file.
func secretsCommonTemplate(d project.Detection) string {
	var b strings.Builder

	b.WriteString(`# buidl secret bindings — shared by every environment.
#
# This file is safe to commit BECAUSE IT CONTAINS NO VALUES. Every line should be
# an indirection to an environment variable supplied at deploy time: GitHub
# Actions secrets in CI, your password manager locally.
#
# If you find yourself pasting a literal value here, stop — put it in
# .buidl/secrets or .buidl/secrets.<environment> instead. Those are gitignored
# precisely because that happens.
#
# Resolution order, later winning:
#   .env, .env.<environment>       (only when env.dotenv: true)
#   .buidl/secrets-common          this file
#   .buidl/secrets                 all environments, gitignored
#   .buidl/secrets.<environment>   one environment, gitignored
#   the process environment        always wins
#
# Declare the NAMES under env.secret in buidl.yaml; this file supplies values.

# DATABASE_URL=$PROD_DATABASE_URL
`)

	if d.Framework == "rails" {
		b.WriteString("# RAILS_MASTER_KEY=$RAILS_MASTER_KEY\n")
	}
	return b.String()
}

// secretsValuesTemplate is the gitignored values file.
func secretsValuesTemplate() string {
	return `# Local secret values for buidl. NEVER commit this file.
#
# Values here apply to every environment. For one environment only, use
# .buidl/secrets.<environment> — for example .buidl/secrets.production.
#
# A leading $ indirects through your environment, so this file can reference a
# password manager shim rather than holding the secret itself:
#   DATABASE_URL=$PROD_DATABASE_URL
`
}

// scaffoldHooks writes sample lifecycle hooks, non-executable so they are inert
// until deliberately enabled.
func (a *App) scaffoldHooks(dir string) error {
	hooksDir := filepath.Join(dir, secrets.Directory, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}

	// Only the points a project is most likely to want; the rest are documented by
	// `buidl hooks`. Written with a .sample suffix and no execute bit so that
	// scaffolding can never silently start running code during a deploy.
	var written []string
	for _, point := range []hooks.Point{hooks.PreDeploy, hooks.PostDeploy} {
		path := filepath.Join(hooksDir, string(point)+".sample")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(hooks.SampleHook(point)), 0o644); err != nil {
			return err
		}
		written = append(written, string(point)+".sample")
	}

	if len(written) > 0 {
		a.log.Success("wrote %s/hooks/{%s}", secrets.Directory, strings.Join(written, ","))
		a.log.Detail("enable one: cp .buidl/hooks/pre-deploy.sample .buidl/hooks/pre-deploy && chmod +x .buidl/hooks/pre-deploy")
	}
	return nil
}

// ensureGitignore appends the secrets path to .gitignore if absent.
//
// This is the one file init modifies rather than creates, because the cost of
// getting it wrong (committed credentials) is so much higher than the cost of a
// duplicate line.
func (a *App) ensureGitignore(dir string) error {
	path := filepath.Join(dir, ".gitignore")

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// The values files must be ignored; secrets-common must NOT be, since being
	// committed is the entire point of it. A bare `.buidl/` rule would wrongly
	// exclude both it and the hooks.
	entries := []string{
		".buidl/secrets",
		".buidl/secrets.*",
	}

	present := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(existing)))
	for scanner.Scan() {
		present[strings.TrimSpace(scanner.Text())] = true
	}

	var missing []string
	for _, entry := range entries {
		if !present[entry] {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	block := prefix + "\n# buidl local secret values (secrets-common is committed on purpose)\n" +
		strings.Join(missing, "\n") + "\n"
	if _, err := f.WriteString(block); err != nil {
		return err
	}
	a.log.Detail("added %s to .gitignore", strings.Join(missing, ", "))
	return nil
}

// resolveInitCI answers the setup-wizard questions: Actions, staging, review
// apps. Flags are the non-interactive answers. On a TTY, unanswered questions
// are asked. Off a TTY, unanswered means no.
func (a *App) resolveInitCI(cmd *cobra.Command, writeCI, noCI, staging, preview bool) (initCIChoice, error) {
	if writeCI && noCI {
		return initCIChoice{}, fmt.Errorf("pass --ci or --no-ci, not both")
	}
	if noCI && (staging || preview) {
		return initCIChoice{}, fmt.Errorf("--no-ci cannot be combined with --staging or --preview")
	}

	// Any setup flag answers the whole wizard so scripts never hang.
	// Remaining questions are only asked when nothing was specified.
	flagged := cmd.Flags().Changed("ci") || cmd.Flags().Changed("no-ci") ||
		cmd.Flags().Changed("staging") || cmd.Flags().Changed("preview")
	interactive := a.canPrompt(cmd) && !flagged

	var c initCIChoice

	// Preview implies the rest: review apps sit on staging and need a workflow.
	if preview {
		return initCIChoice{CI: true, Staging: true, Preview: true}, nil
	}
	if noCI {
		return c, nil
	}

	switch {
	case writeCI || staging:
		c.CI = true
	case interactive:
		a.log.Info("")
		yes, err := a.askYesNo(cmd, "Would you like to set up GitHub Actions?", false)
		if err != nil {
			return c, err
		}
		c.CI = yes
	}
	if !c.CI {
		return c, nil
	}

	switch {
	case staging:
		c.Staging = true
	case interactive:
		yes, err := a.askYesNo(cmd, "Would you like a staging environment?", false)
		if err != nil {
			return c, err
		}
		c.Staging = yes
	}
	if !c.Staging {
		return c, nil
	}

	switch {
	case preview:
		c.Preview = true
	case interactive:
		yes, err := a.askYesNo(cmd, "Would you like review apps (a preview per pull request)?", false)
		if err != nil {
			return c, err
		}
		c.Preview = yes
	}
	return c, nil
}

// applyInitCI writes the workflow and, when asked, the staging / production /
// preview overlays. The user never has to open the file for this.
func (a *App) applyInitCI(dir string, force bool, choice initCIChoice) error {
	if !choice.CI {
		return nil
	}

	if choice.Staging {
		f, err := config.Open(filepath.Join(dir, "buidl.yaml"))
		if err != nil {
			return err
		}
		if err := a.addEnvironment(f, "staging", "", ""); err != nil {
			return err
		}
		if err := a.addEnvironment(f, "production", "", ""); err != nil {
			return err
		}
		if choice.Preview {
			if err := a.addEnvironment(f, "preview", "", ""); err != nil {
				return err
			}
		}
		if err := f.Save(); err != nil {
			return err
		}
		if err := a.validateEditedConfig(f, ""); err != nil {
			return err
		}
		if choice.Preview {
			a.log.Success("wrote staging, production, and preview environments")
		} else {
			a.log.Success("wrote staging and production environments")
		}
		a.log.Detail("default environment is staging; production is a promote")
	}

	return a.scaffoldCI(dir, force, choice)
}

func (a *App) validateInitConfig(configPath string) error {
	f, err := config.Open(configPath)
	if err != nil {
		return fmt.Errorf("the generated config did not validate: %w", err)
	}
	if err := a.validateEditedConfig(f, ""); err != nil {
		return fmt.Errorf("the generated config did not validate: %w", err)
	}
	return nil
}

// scaffoldCI writes a GitHub Actions workflow matching the setup answers.
func (a *App) scaffoldCI(dir string, force bool, choice initCIChoice) error {
	wfDir := filepath.Join(dir, ".github", "workflows")
	path := filepath.Join(wfDir, "deploy.yml")

	if _, err := os.Stat(path); err == nil && !force {
		a.log.Detail("skipping CI workflow; %s already exists", path)
		return nil
	}
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		return err
	}
	body := renderGithubWorkflow(choice.Staging, choice.Preview)
	if err := writeFile(path, body, force); err != nil {
		return err
	}
	a.log.Success("wrote .github/workflows/deploy.yml")
	return nil
}

// writeFile writes content, refusing to clobber unless force is set.
func writeFile(path, content string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists (pass --force to overwrite)", path)
		}
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// validateAllEnvironments validates every declared environment.
func (a *App) validateAllEnvironments(ctx context.Context) error {
	// Interpolation of ${BUIDL_SLUG} and friends depends on git provenance, which
	// requireConfig would normally have loaded.
	if err := a.ensureGit(ctx); err != nil {
		return err
	}

	// Load without an environment to discover the list. A config that requires an
	// environment reports the names in its error, so try the permissive path
	// first.
	res, err := config.Load(config.LoadOptions{
		Path:   a.opts.configPath,
		Strict: true,
		Vars:   a.interpolationVars(a.git),
	})

	var names []string
	switch {
	case err == nil:
		names = res.Environments
		if len(names) == 0 {
			a.log.Success("%s is valid", res.Path)
			return nil
		}
	default:
		// The error names the declared environments; re-derive them by loading
		// each candidate. Parse them out of the message rather than duplicating
		// discovery logic.
		names = environmentsFromError(err)
		if len(names) == 0 {
			return err
		}
	}

	var failures int
	for _, name := range names {
		r, err := config.Load(config.LoadOptions{
			Path:        a.opts.configPath,
			Environment: name,
			Strict:      true,
			Vars:        a.interpolationVars(a.git),
		})
		if err != nil {
			failures++
			a.log.Error("environment %q: %s", name, err)
			continue
		}
		a.log.Success("environment %q is valid (namespace %s)", name, r.Config.Deploy.Kubernetes.Namespace)
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d environments failed validation", failures, len(names))
	}
	return nil
}

// environmentsFromError extracts environment names from the loader's
// "declares environments (a, b)" message.
func environmentsFromError(err error) []string {
	msg := err.Error()
	open := strings.Index(msg, "environments (")
	if open < 0 {
		return nil
	}
	rest := msg[open+len("environments ("):]
	close := strings.Index(rest, ")")
	if close < 0 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(rest[:close], ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
