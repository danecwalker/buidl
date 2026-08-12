package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danewalker/buidl/internal/deploy"
	"github.com/danewalker/buidl/internal/deploy/kubernetes"
)

// newAccessoryCmd manages the supporting stateful services declared under
// `accessories`.
//
// These live behind their own verb rather than riding along with `deploy` on
// purpose. Reconciling a database is not something that should happen because
// someone shipped a web app — see the comment at the top of
// internal/deploy/kubernetes/accessories.go. The cost is that an accessory can
// drift until someone runs this; `accessory plan` is how that drift is seen.
func newAccessoryCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "accessory",
		Aliases: []string{"accessories"},
		Short:   "Manage supporting services (databases, caches, queues)",
		Long: `Manage the services declared under ` + "`accessories`" + ` in buidl.yaml.

Accessories are deliberately not reconciled by ` + "`buidl deploy`" + `. An app
deploy runs many times a day and replaces every pod it owns; a restarted
database must never be a side effect of shipping a web app. Reconciling one is
always something you ask for by name.`,
	}

	cmd.AddCommand(newAccessoryPlanCmd(a), newAccessoryApplyCmd(a))
	return cmd
}

func newAccessoryPlanCmd(a *App) *cobra.Command {
	var detailed bool

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what reconciling the accessories would change",
		Long: `Dry-run the accessories against the API server and report the changes.

This is also how you see drift: because an ordinary deploy leaves accessories
alone, this is the only command that will tell you an accessory no longer
matches its configuration.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			target, req, err := a.accessoryRequest(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			plan, err := target.PlanAccessories(ctx, req)
			if err != nil {
				return err
			}

			a.renderAccessoryPlan(plan, detailed)
			return nil
		},
	}

	cmd.Flags().BoolVar(&detailed, "detailed", false, "include full object diffs")
	return cmd
}

func newAccessoryApplyCmd(a *App) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Reconcile the accessories with their configuration",
		Long: `Apply the configured accessories to the cluster.

This can restart a database. Changing an accessory's image, environment, or
resources replaces its pod, and a StatefulSet pod carries the data volume with
it. Run ` + "`buidl accessory plan`" + ` first; the "effect" column names which
changes restart the accessory and which do not.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			target, req, err := a.accessoryRequest(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			// Confirm against the plan, not against the config: what matters is
			// whether this particular run restarts anything, and a no-op run
			// should not demand a confirmation.
			plan, err := target.PlanAccessories(ctx, req)
			if err != nil {
				return err
			}
			a.renderAccessoryPlan(plan, false)

			if err := a.confirmAccessoryApply(cmd, plan, yes); err != nil {
				return err
			}

			changes, err := target.ApplyAccessories(ctx, req)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				a.log.Success("no accessories to apply")
				return nil
			}

			a.log.EndStep()
			a.log.Success("applied %d accessory object(s)", len(changes))
			// Rollout is not waited on. A StatefulSet with a fresh volume can
			// spend minutes initialising, and buidl has no way to know what
			// "ready" means for an arbitrary database image.
			a.log.Detail("watch it come up with `kubectl rollout status statefulset -n %s -l app.kubernetes.io/component=accessory`",
				a.cfg.Deploy.Kubernetes.Namespace)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// accessoryRequest resolves everything the accessory commands share.
func (a *App) accessoryRequest(ctx context.Context) (*kubernetes.Target, deploy.Request, error) {
	if err := a.requireConfig(ctx); err != nil {
		return nil, deploy.Request{}, err
	}

	if len(a.cfg.Accessories) == 0 {
		return nil, deploy.Request{}, fmt.Errorf("no accessories declared for environment %q\n\n"+
			"hint: add them under `accessories` in %s", a.cfg.Environment, a.path)
	}

	if a.cfg.Deploy.Target != "kubernetes" {
		return nil, deploy.Request{}, fmt.Errorf("accessories are only supported for the kubernetes target")
	}

	secretValues, err := a.resolveSecrets()
	if err != nil {
		return nil, deploy.Request{}, err
	}

	generic, err := a.target()
	if err != nil {
		return nil, deploy.Request{}, err
	}
	target, ok := generic.(*kubernetes.Target)
	if !ok {
		generic.Close()
		return nil, deploy.Request{}, fmt.Errorf("accessories are only supported for the kubernetes target")
	}

	// An accessory image is written by hand and is not part of the app's release
	// identity, so the release carries no digest here. It is still passed so
	// applied objects record who reconciled them and when.
	req := a.deployRequest(a.newRelease(""), secretValues, false, false)
	return target, req, nil
}

// renderAccessoryPlan prints an accessory plan.
//
// It deliberately does not reuse renderPlan: that header reports the release's
// image digest, which is the app's identity and says nothing about an accessory.
func (a *App) renderAccessoryPlan(plan *deploy.Plan, detailed bool) {
	a.log.EndStep()

	for _, w := range plan.Warnings {
		a.log.Warn("%s", w)
	}

	a.log.KeyValues([][2]string{
		{"environment", plan.Environment},
		{"cluster", a.clusterDescription()},
	})
	a.log.Info("")

	if !plan.HasChanges() {
		a.log.Success("no changes; the accessories match your configuration")
		return
	}

	rows := make([][]string, 0, len(plan.Changes))
	for _, c := range plan.Changes {
		rows = append(rows, []string{
			actionMarker(c.Action),
			c.Kind,
			c.Name,
			c.FieldSummary(),
			c.Impact,
		})
	}
	a.log.Table([]string{"", "kind", "name", "changes", "effect"}, rows)

	counts := plan.Counts()
	a.log.Info("")
	a.log.Info("plan: %d to create, %d to update, %d unchanged",
		counts[deploy.ActionCreate], counts[deploy.ActionUpdate], counts[deploy.ActionUnchanged])

	if detailed {
		for _, c := range plan.Changes {
			if c.Diff == "" {
				continue
			}
			a.log.Info("")
			a.log.Info("--- %s/%s", c.Kind, c.Name)
			a.log.Indented("  ", c.Diff)
		}
	} else {
		a.log.Detail("pass --detailed for full object diffs")
	}
}

// confirmAccessoryApply prompts before anything that restarts an accessory.
//
// The prompt is keyed on impact rather than on the environment: a restarted
// staging database still loses every connection and can still be mid-write, and
// unlike an app rollout there is no second replica to serve traffic while it
// comes back. A run that only creates new objects, or changes nothing, needs no
// confirmation.
func (a *App) confirmAccessoryApply(cmd *cobra.Command, plan *deploy.Plan, yes bool) error {
	if yes || a.log.CI().Detected || a.log.Mode() != "pretty" {
		return nil
	}

	var restarting []string
	for _, c := range plan.Changes {
		if c.Action == deploy.ActionUpdate && c.Impact != "" {
			restarting = append(restarting, c.Name)
		}
	}
	if len(restarting) == 0 {
		return nil
	}

	a.log.Info("")
	a.log.Warn("this will restart %d accessory pod(s): %s", len(restarting), strings.Join(restarting, ", "))
	return a.confirm(cmd, "Reconcile the accessories?", "accessory apply cancelled")
}
