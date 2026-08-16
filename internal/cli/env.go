package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/danecwalker/buidl/internal/hooks"
)

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
		Use:    "hooks",
		Hidden: true,
		Short:  "List the lifecycle hooks buidl will run",
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
