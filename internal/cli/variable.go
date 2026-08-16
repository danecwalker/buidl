package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/secrets"
)

// newVariableCmd inspects and writes the variables a release will run with.
func newVariableCmd(a *App) *cobra.Command {
	list := newVariableListCmd(a)
	cmd := &cobra.Command{
		Use:     "variable",
		Aliases: []string{"var", "vars", "variables"},
		Hidden:  true,
		Short:   "Inspect and set the variables a release will run with",
		Long: `Show the configuration a release will receive, and where each value comes from.

Values are never printed. Secrets are reported by name, source and length only,
because this output is the kind of thing that ends up pasted into a chat or a
ticket. Accessory secrets such as POSTGRES_PASSWORD are listed even when they
are not declared under the app's env.secret.

  buidl variable list
  buidl variable set DATABASE_URL=postgres://...
  buidl variable set LOG_LEVEL=debug --clear
  buidl variable delete DATABASE_URL

This used to live under ` + "`buidl env`" + `. That name now manages environments.`,
		Args: cobra.NoArgs,
		RunE: list.RunE,
	}
	cmd.AddCommand(list, newVariableSetCmd(a), newVariableDeleteCmd(a))
	return cmd
}

func newVariableListCmd(a *App) *cobra.Command {
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
			a.applyAccessoryURLs(res)

			for _, w := range res.Warnings {
				a.log.Warn("%s", w)
			}

			a.log.KeyValues([][2]string{
				{"environment", a.cfg.Environment},
				{"files read", strings.Join(res.Files, ", ")},
			})
			a.log.Info("")

			clearNames := make([]string, 0, len(a.cfg.Env.Clear))
			for name := range a.cfg.Env.Clear {
				clearNames = append(clearNames, name)
			}
			sort.Strings(clearNames)

			secretNames := a.cfg.SecretNames()
			rows := make([][]string, 0, len(clearNames)+len(secretNames))
			for _, name := range clearNames {
				rows = append(rows, []string{name, "clear", "buidl.yaml", a.cfg.Env.Clear[name]})
			}

			declared := append([]string(nil), secretNames...)
			sort.Strings(declared)
			appSecrets := map[string]bool{}
			for _, name := range a.cfg.Env.Secret {
				appSecrets[name] = true
			}
			for _, name := range declared {
				source := "MISSING"
				shape := "-"
				if value, ok := res.Values[name]; ok {
					source = string(res.Sources[name])
					shape = fmt.Sprintf("set, %d chars", len(value))
				}
				kind := "secret"
				if !appSecrets[name] {
					kind = "accessory"
				}
				rows = append(rows, []string{name, kind, source, shape})
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

func newVariableSetCmd(a *App) *cobra.Command {
	var clear bool

	cmd := &cobra.Command{
		Use:   "set NAME=value",
		Short: "Set a variable for the next deploy",
		Long: `Write a variable so the next deploy can resolve it.

By default the value goes in .buidl/secrets (or .buidl/secrets.<env> with -e)
and the name is declared under env.secret. Pass --clear to put a non-secret
value in buidl.yaml instead.

  buidl variable set DATABASE_URL=postgres://...
  buidl variable set -e production DATABASE_URL=postgres://prod
  buidl variable set LOG_LEVEL=debug --clear`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, value, err := parseAssignment(args)
			if err != nil {
				return err
			}
			if !config.ValidEnvVar(name) {
				return fmt.Errorf("%q is not a valid environment variable name", name)
			}

			f, err := a.openConfigFile()
			if err != nil {
				return err
			}

			if clear {
				if err := f.RemoveFromSequence([]string{"env", "secret"}, name); err != nil {
					return err
				}
				path := []string{"env", "clear", name}
				if env := a.opts.environment; env != "" {
					if f.Lookup("environments", env) == nil {
						return fmt.Errorf("unknown environment %q (declared: %s)", env, strings.Join(f.EnvironmentNames(), ", "))
					}
					path = []string{"environments", env, "env", "clear", name}
				}
				if err := f.SetString(path, value); err != nil {
					return err
				}
				if err := f.Save(); err != nil {
					return err
				}
				if err := a.validateEditedConfig(f, ""); err != nil {
					return err
				}
				a.log.Success("set %s in %s", name, a.path)
				return nil
			}

			if err := f.AppendUnique([]string{"env", "secret"}, name); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			if err := a.validateEditedConfig(f, ""); err != nil {
				return err
			}

			rel, err := secrets.Set(a.root, a.opts.environment, name, value)
			if err != nil {
				return err
			}
			a.log.Success("set %s in %s (%d chars)", name, rel, len(value))
			return nil
		},
	}

	cmd.Flags().BoolVar(&clear, "clear", false, "write the value to buidl.yaml as a non-secret")
	return cmd
}

func newVariableDeleteCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete NAME",
		Aliases: []string{"rm", "unset"},
		Short:   "Remove a variable declaration and its stored value",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !config.ValidEnvVar(name) {
				return fmt.Errorf("%q is not a valid environment variable name", name)
			}

			f, err := a.openConfigFile()
			if err != nil {
				return err
			}

			if a.opts.environment == "" {
				_ = f.RemoveFromSequence([]string{"env", "secret"}, name)
				_ = f.Delete("env", "clear", name)
			} else {
				env := a.opts.environment
				_ = f.RemoveFromSequence([]string{"environments", env, "env", "secret"}, name)
				_ = f.Delete("environments", env, "env", "clear", name)
			}
			if err := f.Save(); err != nil {
				return err
			}

			if err := secrets.Unset(a.root, a.opts.environment, name); err != nil {
				return err
			}
			a.log.Success("removed %s", name)
			return nil
		},
	}
	return cmd
}

func parseAssignment(args []string) (name, value string, err error) {
	if len(args) == 2 {
		return args[0], args[1], nil
	}
	name, value, ok := strings.Cut(args[0], "=")
	if !ok || name == "" {
		return "", "", fmt.Errorf("expected NAME=value, got %q", args[0])
	}
	return name, value, nil
}
