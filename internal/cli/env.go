package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danewalker/buidl/internal/hooks"
	"github.com/danewalker/buidl/internal/secrets"
)

// newEnvCmd inspects the environment an app will be deployed with.
func newEnvCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Inspect the environment variables a release will run with",
		Long: `Show the configuration a release will receive, and where each value comes from.

Values are never printed. Secrets are reported by name, source and length only,
because this output is the kind of thing that ends up pasted into a chat or a
ticket.`,
	}
	cmd.AddCommand(newEnvListCmd(a))
	return cmd
}

func newEnvListCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the variables a release will run with and where they resolve from",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if err := a.requireConfig(ctx); err != nil {
				return err
			}

			res, err := secrets.Resolve(a.secretOptions())
			if err != nil {
				return err
			}

			for _, w := range res.Warnings {
				a.log.Warn("%s", w)
			}

			a.log.KeyValues([][2]string{
				{"environment", a.cfg.Environment},
				{"files read", strings.Join(res.Files, ", ")},
			})
			a.log.Info("")

			// Plain values, rendered into the Deployment and visible in any diff.
			clearNames := make([]string, 0, len(a.cfg.Env.Clear))
			for name := range a.cfg.Env.Clear {
				clearNames = append(clearNames, name)
			}
			sort.Strings(clearNames)

			rows := make([][]string, 0, len(clearNames)+len(a.cfg.Env.Secret))
			for _, name := range clearNames {
				rows = append(rows, []string{name, "clear", "buidl.yaml", a.cfg.Env.Clear[name]})
			}

			// Secret values: name, source and length only.
			declared := append([]string(nil), a.cfg.Env.Secret...)
			sort.Strings(declared)
			for _, name := range declared {
				source := "MISSING"
				shape := "-"
				if value, ok := res.Values[name]; ok {
					source = string(res.Sources[name])
					shape = fmt.Sprintf("set, %d chars", len(value))
				}
				rows = append(rows, []string{name, "secret", source, shape})
			}

			a.log.Table([]string{"name", "kind", "source", "value"}, rows)

			for _, ref := range a.cfg.Env.SecretRefs {
				a.log.Info("")
				a.log.Info("mounted Secret (managed elsewhere): %s", ref)
			}

			if len(res.Missing) > 0 {
				a.log.Info("")
				a.log.Warn("%d secret(s) have no value and the deploy will fail: %s",
					len(res.Missing), strings.Join(res.Missing, ", "))
			}

			// Names present in .env but not declared are not deployed. Saying so is
			// the whole point: it is the difference between "works locally" and
			// "works in the cluster".
			if len(res.Discovered) > 0 {
				a.log.Info("")
				a.log.Info("Found in dotenv files but NOT deployed (%d):", len(res.Discovered))
				a.log.Indented("  ", strings.Join(wrapNames(res.Discovered, 4), "\n"))
				a.log.Info("")
				a.log.Info("add a name to env.secret (or env.clear) in buidl.yaml to deploy it")
			}

			if !a.cfg.Env.Dotenv {
				a.log.Info("")
				a.log.Detail("env.dotenv is off; .env files are not read")
			}

			// Hooks receive these values too, so list which ones exist.
			if available := a.hookRunner().Available(); len(available) > 0 {
				names := make([]string, 0, len(available))
				for _, p := range available {
					names = append(names, string(p))
				}
				a.log.Info("")
				a.log.Info("hooks receiving these values: %s", strings.Join(names, ", "))
			}

			return nil
		},
	}
	return cmd
}

// wrapNames groups names into lines of n for compact display.
func wrapNames(names []string, perLine int) []string {
	var lines []string
	for i := 0; i < len(names); i += perLine {
		end := min(i+perLine, len(names))
		lines = append(lines, strings.Join(names[i:end], ", "))
	}
	return lines
}

// newHooksCmd lists the lifecycle hooks buidl will run.
func newHooksCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "List the lifecycle hooks buidl will run",
		Long: `Show every hook point and whether an executable is present for it.

Hooks are plain executables in .buidl/hooks named for the lifecycle point. They
receive the release's identity and every resolved secret in their environment,
which is what makes a pre-deploy migration possible without giving the
application itself a schema-altering credential.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if err := a.requireConfig(ctx); err != nil {
				return err
			}

			runner := a.hookRunner()
			a.log.KeyValues([][2]string{{"hooks directory", a.cfg.HooksPath}})
			a.log.Info("")

			present := map[hooks.Point]bool{}
			for _, p := range runner.Available() {
				present[p] = true
			}

			rows := make([][]string, 0, len(hooks.Points()))
			for _, point := range hooks.Points() {
				status := "-"
				if present[point] {
					status = "enabled"
				}
				aborts := "no"
				if point.Aborts() {
					aborts = "yes"
				}
				rows = append(rows, []string{status, string(point), aborts, point.Description()})
			}
			a.log.Table([]string{"", "hook", "aborts deploy", "runs"}, rows)

			if len(present) == 0 {
				a.log.Info("")
				a.log.Info("no hooks are enabled; create one and make it executable:")
				a.log.Info("  chmod +x %s/pre-deploy", a.cfg.HooksPath)
			}
			return nil
		},
	}
	return cmd
}
