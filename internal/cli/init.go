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
		noCI     bool
		noDocker bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Detect the project and scaffold buidl.yaml",
		Long: `Inspect the current directory and write a working configuration.

Detection covers Go, Node, Python, Ruby, Rust and static sites. If there is no
Dockerfile, a multi-stage one is generated for the detected stack. Everything
written is a starting point meant to be edited and committed — buidl never
regenerates these files behind your back.`,
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

			if !noCI {
				if err := a.scaffoldCI(dir, force); err != nil {
					return err
				}
			}

			// Validate what we just wrote, so init never leaves a broken config.
			if _, err := config.Load(config.LoadOptions{
				Path:        configPath,
				Environment: "staging",
				Strict:      true,
				Vars:        map[string]string{"BUIDL_SLUG": "example"},
			}); err != nil {
				return fmt.Errorf("the generated config did not validate: %w", err)
			}

			a.log.EndStep()
			a.log.Success("ready")
			a.log.Info("")
			a.log.Info("next steps:")
			a.log.Info("  1. review buidl.yaml (set proxy.host and env)")
			a.log.Info("  2. buidl config validate")
			a.log.Info("  3. buidl deploy -e staging")
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&appName, "app", "", "application name (default: detected)")
	f.StringVar(&image, "image", "", "image repository (e.g. ghcr.io/acme/web)")
	f.StringVar(&registry, "registry", "", "registry host to build the image reference from (e.g. ghcr.io/acme)")
	f.BoolVar(&force, "force", false, "overwrite existing files")
	f.BoolVar(&noCI, "no-ci", false, "skip writing a CI workflow")
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

build:
  # buildkit builds without a Docker daemon and pushes straight to the registry.
  driver: buildkit
  dockerfile: %s
  # Add linux/arm64 here to publish a multi-arch image.
  platforms: [linux/amd64]

deploy:
  target: kubernetes
  port: %d
  replicas: 2

  healthcheck:
    # The rollout is gated on this endpoint, so it must return 200 only when the
    # app is genuinely ready to serve.
    path: %s

  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {memory: 512Mi}

  strategy:
    # maxUnavailable 0 keeps full capacity during a rollout.
    type: rolling
    maxUnavailable: "0"
    maxSurge: 25%%

env:
  clear:
    LOG_LEVEL: info
  # Names only — values are read from the environment or .buidl/secrets at
  # deploy time and are never written to this file.
  secret: []
`, config.SchemaVersion, d.Name, image, dockerfilePath(d), d.Port, healthPath(d))

	fmt.Fprintf(&b, `
# Environments overlay the settings above. Deploy one with: buidl deploy -e staging
environments:
  staging:
    deploy:
      replicas: 1
      kubernetes:
        namespace: %s-staging
        createNamespace: true
    proxy:
      host: staging.example.com
      ssl: true
    env:
      clear:
        LOG_LEVEL: debug

  production:
    deploy:
      replicas: 3
      kubernetes:
        namespace: %s
      # Fail the deploy and revert if the new release never becomes healthy.
      deployTimeout: 10m
      autoscale:
        min: 3
        max: 10
        cpuPercent: 70
    proxy:
      host: example.com
      ssl: true

  # Per-branch preview environments. BUIDL_SLUG is derived from the branch name
  # (or the PR number in CI), so a new branch needs no configuration at all.
  # buidl destroy -e preview deletes the namespace when the PR closes.
  preview:
    deploy:
      replicas: 1
      kubernetes:
        namespace: %s-preview-${BUIDL_SLUG}
        createNamespace: true
        ephemeral: true
    proxy:
      host: ${BUIDL_SLUG}.preview.example.com
      ssl: true
`, d.Name, d.Name, d.Name)

	return b.String()
}

func dockerfilePath(d project.Detection) string {
	if d.DockerfilePath != "" {
		return d.DockerfilePath
	}
	return "Dockerfile"
}

func healthPath(d project.Detection) string {
	if d.HealthPath != "" {
		return d.HealthPath
	}
	return "/up"
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

// scaffoldCI writes a GitHub Actions workflow.
func (a *App) scaffoldCI(dir string, force bool) error {
	wfDir := filepath.Join(dir, ".github", "workflows")
	path := filepath.Join(wfDir, "deploy.yml")

	if _, err := os.Stat(path); err == nil && !force {
		a.log.Detail("skipping CI workflow; %s already exists", path)
		return nil
	}
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		return err
	}
	if err := writeFile(path, githubWorkflow, force); err != nil {
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
