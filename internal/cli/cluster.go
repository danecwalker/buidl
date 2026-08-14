package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/danecwalker/buidl/internal/cluster"
	"github.com/danecwalker/buidl/internal/inventory"
)

// newClusterCmd groups the commands that turn servers into a cluster.
func newClusterCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Create and manage the Kubernetes cluster behind an environment",
		Long: `Turn a fleet of servers into a Kubernetes cluster buidl can deploy to.

buidl does not provision infrastructure. Creating machines, networks and firewalls
is the job of OpenTofu, Terraform, Ansible or your cloud console — tools that own
that state and do it well. buidl takes servers that already exist, installs
Kubernetes on them, joins them into one cluster, and hands you a kubeconfig.

List the machines your provisioning tool produced:

  infra:
    kubernetes:
      distribution: k3s          # or rke2
    servers:
      - {host: 10.0.0.1, role: control-plane}
      - {host: 10.0.0.2, role: worker}

There is no separate "create the cluster" command. ` + "`buidl plan`" + ` inspects the
servers and includes any Kubernetes installation the fleet needs, and
` + "`buidl deploy`" + ` converges the cluster before shipping the app. A fresh set of
servers and a running cluster take the same command.

The subcommands here are for inspection and teardown:

  buidl cluster inventory   show the resolved fleet
  buidl cluster status      report node health
  buidl cluster kubeconfig  fetch credentials into ~/.kube/config
  buidl cluster reset       uninstall Kubernetes (destructive)`,
	}

	cmd.AddCommand(
		newClusterStatusCmd(a),
		newClusterKubeconfigCmd(a),
		newClusterInventoryCmd(a),
		newClusterResetCmd(a),
	)
	return cmd
}

// clusterManager loads config and builds a cluster manager.
func (a *App) clusterManager(cmd *cobra.Command) (*cluster.Manager, error) {
	ctx := cmd.Context()
	if ctx == nil {
		return nil, fmt.Errorf("no command context")
	}
	if err := a.requireConfig(ctx); err != nil {
		return nil, err
	}
	return cluster.New(a.cfg.Infra, a.log)
}

// clusterPlan computes the cluster plan, or returns nil when no infra is
// configured.
//
// Returning nil rather than an error is what lets `plan` and `deploy` work
// unchanged against a managed cluster: no infra block simply means buidl has no
// servers to manage.
func (a *App) clusterPlan(ctx context.Context) (*cluster.Manager, *cluster.Plan, error) {
	if a.cfg.Infra == nil {
		return nil, nil, nil
	}

	mgr, err := cluster.New(a.cfg.Infra, a.log)
	if err != nil {
		return nil, nil, err
	}

	plan, err := mgr.Plan(ctx)
	if err != nil {
		mgr.Close()
		return nil, nil, err
	}
	return mgr, plan, nil
}

// adoptManagedContext points the loaded config at the managed cluster and
// fetches kubeconfig if this machine does not already have it.
//
// mgr is the manager the caller already opened (plan and deploy inspect the
// fleet first). Reusing it avoids a second SSH session just to copy
// credentials. Pass nil only when the context is already local.
func (a *App) adoptManagedContext(cmd *cobra.Command, mgr *cluster.Manager) error {
	if a.cfg == nil || a.cfg.Infra == nil || a.cfg.Deploy.Kubernetes.Context != "" {
		return nil
	}

	contextName := a.defaultContextName()
	if !cluster.ContextExists(contextName) {
		if mgr == nil {
			return fmt.Errorf("no local credentials for the %s cluster (kubeconfig context %q not found)",
				a.cfg.Environment, contextName)
		}
		a.log.Detail("no local credentials for %s; fetching", contextName)
		if err := a.fetchKubeconfig(cmd, mgr, contextName, "", false); err != nil {
			return fmt.Errorf("cannot obtain credentials for the %s cluster: %w", a.cfg.Environment, err)
		}
	}
	a.cfg.Deploy.Kubernetes.Context = contextName
	return nil
}

// ensureClusterCredentials makes sure this machine can address the managed
// cluster for the currently loaded environment.
//
// Unlike convergeCluster this installs nothing — it only resolves, and if
// necessary fetches, the kubeconfig context. Commands that operate on an
// existing cluster need that: without it they silently fall through to whatever
// context happens to be current, which on a colleague's laptop or a CI runner is
// a different cluster entirely. Reading the wrong cluster's state is worse than
// failing, because the answer looks plausible.
func (a *App) ensureClusterCredentials(cmd *cobra.Command) error {
	if a.cfg == nil || a.cfg.Infra == nil || a.cfg.Deploy.Kubernetes.Context != "" {
		return nil
	}
	if cluster.ContextExists(a.defaultContextName()) {
		return a.adoptManagedContext(cmd, nil)
	}

	mgr, err := cluster.New(a.cfg.Infra, a.log)
	if err != nil {
		return err
	}
	defer mgr.Close()
	return a.adoptManagedContext(cmd, mgr)
}

// errClusterUnknown is returned when no server could be inspected.
//
// This must be a hard error rather than a warning: with nothing known about any
// machine, buidl cannot tell a healthy cluster from an empty fleet, and
// continuing would produce a confusing failure further downstream.
func errClusterUnknown() error {
	return fmt.Errorf("could not reach any server in the fleet, so the cluster's state is unknown\n\n" +
		"hint: check infra.servers and SSH access, or pass --skip-cluster to work against an existing cluster")
}

// convergeCluster brings the cluster to its planned state and wires up
// credentials, so an app deploy can proceed against it.
//
// This is the path that replaces a separate bootstrap command: a fresh fleet and an
// established cluster take the same command, because the plan already knows the
// difference.
func (a *App) convergeCluster(cmd *cobra.Command, mgr *cluster.Manager, plan *cluster.Plan, yes bool) error {
	ctx := cmd.Context()

	// An unreachable machine usually means a wrong address or a closed firewall.
	// Proceeding would half-build the cluster.
	if unreachable := plan.Unreachable(); len(unreachable) > 0 && !yes {
		hosts := make([]string, 0, len(unreachable))
		for _, n := range unreachable {
			hosts = append(hosts, n.Server.Host)
		}
		return fmt.Errorf("refusing to change the cluster with %d unreachable server(s): %s\n\n"+
			"fix connectivity, or pass --yes to proceed with the reachable ones",
			len(hosts), strings.Join(hosts, ", "))
	}

	if err := a.confirmClusterChange(cmd, plan, yes); err != nil {
		return err
	}

	result, err := mgr.Converge(ctx, plan)
	// Report per-node outcomes either way: on failure, which machines were already
	// changed is exactly what the user needs to understand the fleet's state.
	a.reportClusterResult(result)
	if err != nil {
		return err
	}

	if err := mgr.ApplyAddons(ctx); err != nil {
		return err
	}

	// Fetch credentials and point the deploy at the cluster we just built.
	contextName := a.defaultContextName()
	if err := a.fetchKubeconfig(cmd, mgr, contextName, "", false); err != nil {
		return fmt.Errorf("the cluster converged but fetching its kubeconfig failed: %w", err)
	}

	// Without this, a deploy would target whatever context happened to be
	// current, which after building a new cluster is almost never what was meant.
	if a.cfg.Deploy.Kubernetes.Context == "" {
		a.cfg.Deploy.Kubernetes.Context = contextName
		a.log.Detail("targeting kubeconfig context %s", contextName)
	}

	if a.cfg.Infra.Addons.BuildKit && a.cfg.Build.Addr == "" {
		a.log.Info("")
		a.log.Info("an in-cluster builder is available; point builds at it:")
		a.log.Info("  build:")
		a.log.Info("    addr: %s", cluster.BuildKitAddress())
	}

	return nil
}

// reportClusterResult prints what convergence did to each machine.
//
// Cluster changes are not atomic across a fleet, so a failure partway through
// leaves some machines changed and others not. Naming them individually is the
// only way a user can reason about where the fleet actually is.
func (a *App) reportClusterResult(result *cluster.Result) {
	if result == nil || len(result.Outcomes) == 0 {
		return
	}

	rows := make([][]string, 0, len(result.Outcomes))
	for _, o := range result.Outcomes {
		status := "ok"
		detail := ""
		if !o.OK() {
			status = "FAILED"
			detail = truncate(firstLineOf(o.Err.Error()), 50)
		}
		rows = append(rows, []string{
			status,
			o.Host,
			string(o.Role),
			string(o.Action),
			o.Duration.Round(time.Second).String(),
			detail,
		})
	}

	a.log.EndStep()
	a.log.Info("")
	a.log.Info("Server changes")
	a.log.Table([]string{"status", "host", "role", "action", "took", "detail"}, rows)

	if failed := result.Failed(); len(failed) > 0 {
		a.log.Warn("%d of %d server(s) failed; the fleet is in a mixed state",
			len(failed), len(result.Outcomes))
		if succeeded := result.Succeeded(); len(succeeded) > 0 {
			items := make([]string, 0, len(succeeded))
			for _, o := range succeeded {
				items = append(items, fmt.Sprintf("%s (%s)", o.Host, o.Action))
			}
			a.log.Bullets("already changed", items)
		}
		a.log.Info("re-run once the cause is fixed; convergence skips servers that are already correct")
	}
}

// firstLineOf returns the first line of a multi-line message, for a table cell.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// renderClusterPlan prints a cluster plan.
func (a *App) renderClusterPlan(plan *cluster.Plan, showConfig bool) {
	a.log.EndStep()

	version := plan.Kubernetes.Version
	if version == "" {
		version = "(unpinned, latest stable)"
	}

	a.log.KeyValues([][2]string{
		{"distribution", plan.Distro.Name() + " " + version},
		{"topology", plan.Inventory.Summary()},
		{"registration", plan.RegistrationAddress},
		{"addons", addonPlanSummary(plan)},
	})
	a.log.Info("")

	rows := make([][]string, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		rows = append(rows, []string{
			actionMarkerFor(node.Action),
			node.Server.Host,
			string(node.Role),
			hostFacts(node),
			installedVersion(node),
			string(node.Action),
			node.Reason,
		})
	}
	a.log.Table([]string{"", "host", "role", "system", "installed", "action", "detail"}, rows)

	for _, w := range plan.Warnings {
		a.log.Warn("%s", w)
	}

	if showConfig {
		for _, node := range plan.Nodes {
			if node.Config == "" {
				continue
			}
			a.log.Info("")
			a.log.Info("--- %s: %s", node.Server.Host, plan.Distro.ConfigPath())
			for _, line := range strings.Split(strings.TrimRight(node.Config, "\n"), "\n") {
				// The config embeds the join token, which is a cluster-admin
				// equivalent credential; never print it.
				if strings.HasPrefix(strings.TrimSpace(line), "token:") {
					a.log.Info("  token: <redacted>")
					continue
				}
				a.log.Info("  %s", line)
			}
		}
	}

	a.log.Info("")

	skipped := plan.Skipped()
	pending := plan.PendingAddons()

	// Claiming "no changes" when nothing could be inspected would be a lie, and
	// the most dangerous kind: it reads as a healthy cluster. Callers turn this
	// into a hard error via errClusterUnknown.
	if !plan.Actionable() {
		// With every machine unreachable the table's truncated detail is all the
		// user has; print the full error for at least one so the cause is legible.
		for _, node := range plan.Skipped() {
			if node.Facts.Error != nil {
				a.log.Info("")
				a.log.Info("why %s could not be reached:", node.Server.Host)
				a.log.Indented("  ", node.Facts.Error.Error())
				break
			}
		}
		return
	}

	// An addon a deploy would install is a change, and a large one: cert-manager
	// installs cluster-wide CRDs and takes minutes. Reporting it here is what stops
	// a deploy from doing that unannounced.
	if len(pending) > 0 {
		names := make([]string, 0, len(pending))
		for _, entry := range pending {
			names = append(names, entry.Addon.Name)
		}
		a.log.Info("plan: %d addon(s) to install: %s", len(pending), strings.Join(names, ", "))
	}

	switch {
	case !plan.HasChanges() && len(skipped) == 0 && len(pending) == 0:
		a.log.Success("no changes; the cluster matches your configuration")
	case !plan.HasChanges() && len(skipped) == 0:
		a.log.Info("no server changes; the addon(s) above are still to install")
	case !plan.HasChanges():
		a.log.Success("no changes on the %d server(s) that could be inspected", len(plan.Nodes)-len(skipped))
		a.log.Warn("%d server(s) were skipped and remain unknown", len(skipped))
	default:
		a.log.Info("plan: %d of %d server(s) need changes", len(plan.Changes()), len(plan.Nodes))
		if len(skipped) > 0 {
			a.log.Warn("%d server(s) were skipped and will not be changed", len(skipped))
		}
	}
}

// actionMarkerFor renders a cluster action as a diff-style marker, so the shape
// of a plan is readable at a glance.
func actionMarkerFor(action cluster.Action) string {
	switch action {
	case cluster.ActionBootstrap, cluster.ActionJoinControlPlane, cluster.ActionJoinWorker:
		return "+"
	case cluster.ActionReconfigure, cluster.ActionUpgrade:
		return "~"
	case cluster.ActionSkipped:
		return "!"
	default:
		return " "
	}
}

// installedVersion renders the version already on a machine.
func installedVersion(node cluster.NodePlan) string {
	if !node.Facts.Reachable {
		return "-"
	}
	if !node.Facts.Installed {
		return "none"
	}
	version := node.Facts.CurrentVersion
	if version == "" {
		return "unknown"
	}
	// `k3s --version` prints "k3s version v1.33.5+k3s1 (hash)". Matching a leading
	// "v" alone would return the literal word "version", so require a digit after
	// it.
	for _, field := range strings.Fields(version) {
		if len(field) > 1 && field[0] == 'v' && field[1] >= '0' && field[1] <= '9' {
			return field
		}
	}
	return truncate(version, 20)
}

// addonPlanSummary lists each enabled addon with whether the cluster has it.
//
// The state belongs on this line: a bare list of names reads as a description of
// the cluster, so a header saying `addons cert-manager` next to "no changes" was
// claiming an addon was in place that had never been installed.
func addonPlanSummary(plan *cluster.Plan) string {
	if len(plan.Addons) == 0 {
		return "none"
	}
	names := make([]string, 0, len(plan.Addons))
	for _, entry := range plan.Addons {
		state := "pending"
		if entry.Installed {
			state = "installed"
		}
		names = append(names, entry.Addon.Name+" ("+state+")")
	}
	return strings.Join(names, ", ")
}

// hostFacts renders a machine's discovered system info.
func hostFacts(node cluster.NodePlan) string {
	f := node.Facts
	if !f.Reachable {
		return "unreachable"
	}
	parts := []string{}
	if f.OS != "" {
		parts = append(parts, f.OS)
	}
	if f.Arch != "" {
		parts = append(parts, f.Arch)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "/")
}

// confirmClusterChange prompts before installing software on servers.
func (a *App) confirmClusterChange(cmd *cobra.Command, plan *cluster.Plan, yes bool) error {
	if yes || a.log.CI().Detected || a.log.Mode() != "pretty" {
		return nil
	}

	changes := plan.Changes()
	fmt.Fprintf(cmd.OutOrStdout(),
		"\nThis will install %s on %d server(s) as root.\nContinue? [y/N] ",
		plan.Distro.Name(), len(changes))

	var answer string
	if _, err := fmt.Fscanln(cmd.InOrStdin(), &answer); err != nil {
		return fmt.Errorf("cancelled")
	}
	switch answer {
	case "y", "Y", "yes", "Yes":
		return nil
	default:
		return fmt.Errorf("cancelled")
	}
}

// defaultContextName names the kubeconfig context after the app and environment,
// so managing several clusters stays legible.
func (a *App) defaultContextName() string {
	if a.cfg.Environment == "" || a.cfg.Environment == "default" {
		return a.cfg.App
	}
	return a.cfg.App + "-" + a.cfg.Environment
}

// newClusterStatusCmd reports node health.
func newClusterStatusCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report the health of the cluster's nodes",
		Long: `Report every server in the inventory and its state in the cluster.

Machines that never joined are listed too, with the reason — a server that is
unreachable or whose service is stopped is exactly what you need to see, and
reporting only what the API server knows would hide it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			mgr, err := a.clusterManager(cmd)
			if err != nil {
				return err
			}
			defer mgr.Close()

			st, err := mgr.Status(ctx)
			if err != nil {
				return err
			}

			a.log.EndStep()
			a.log.Info("distribution  %s", st.Distribution)
			if st.Endpoint != "" {
				a.log.Info("endpoint      %s", st.Endpoint)
			}
			a.log.Info("topology      %s", st.Summary)
			if !st.Reachable {
				a.log.Warn("could not query the API server; node states below come from the servers themselves")
			}
			a.log.Info("")

			rows := make([][]string, 0, len(st.Nodes))
			for _, n := range st.Nodes {
				rows = append(rows, []string{
					n.Host,
					orDash(n.Name),
					string(n.Role),
					n.Status,
					orDash(n.Version),
					truncate(n.Message, 40),
				})
			}
			a.log.Table([]string{"host", "node", "role", "status", "version", "message"}, rows)

			for _, w := range st.Warnings {
				a.log.Warn("%s", w)
			}
			return nil
		},
	}
	return cmd
}

// newClusterKubeconfigCmd fetches cluster credentials.
func newClusterKubeconfigCmd(a *App) *cobra.Command {
	var (
		merge       bool
		output      string
		contextName string
		setCurrent  bool
	)

	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Fetch the cluster's kubeconfig",
		Long: `Fetch admin credentials from a control-plane server.

The on-node kubeconfig points at 127.0.0.1, which only works on that machine, so
the server address is rewritten to the control-plane endpoint (or the server's own
address). By default the result is merged into your existing kubeconfig rather
than replacing it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			mgr, err := a.clusterManager(cmd)
			if err != nil {
				return err
			}
			defer mgr.Close()

			name := contextName
			if name == "" {
				name = a.defaultContextName()
			}

			if !merge {
				cfg, err := mgr.Kubeconfig(ctx, name)
				if err != nil {
					return err
				}
				raw, err := clientcmd.Write(*cfg)
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), string(raw))
				return nil
			}

			return a.fetchKubeconfig(cmd, mgr, name, output, setCurrent)
		},
	}

	f := cmd.Flags()
	f.BoolVar(&merge, "merge", true, "merge into the local kubeconfig instead of printing it")
	f.StringVar(&output, "path", "", "kubeconfig file to merge into (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVar(&contextName, "context-name", "", "name for the context (default: <app>-<environment>)")
	f.BoolVar(&setCurrent, "set-current", false, "make this the current context")

	return cmd
}

// fetchKubeconfig retrieves and merges cluster credentials.
func (a *App) fetchKubeconfig(cmd *cobra.Command, mgr *cluster.Manager, contextName, path string, setCurrent bool) error {
	ctx := cmd.Context()

	a.log.Step("Fetching kubeconfig")
	cfg, err := mgr.Kubeconfig(ctx, contextName)
	if err != nil {
		return err
	}

	written, err := cluster.MergeKubeconfig(cfg, path, setCurrent)
	if err != nil {
		return err
	}

	a.log.Success("merged context %q into %s", contextName, written)
	if !setCurrent {
		a.log.Info("use it with: kubectl --context %s get nodes", contextName)
		a.log.Info("or pin it in buidl.yaml: deploy.kubernetes.context: %s", contextName)
	}
	return nil
}

// newClusterInventoryCmd prints the resolved server list.
func newClusterInventoryCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "inventory",
		Aliases: []string{"servers"},
		Short:   "Print the resolved server inventory",
		Long: `Print the fleet after defaults and role inference have been applied.

Useful for confirming what buidl thinks it will act on before running
` + "`buidl deploy`" + ` — particularly which machine will become the bootstrap
control plane.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if a.cfg.Infra == nil {
				return fmt.Errorf("no `infra` block in buidl.yaml")
			}

			inv, err := a.cfg.Infra.InventoryProvider().Resolve(ctx)
			if err != nil {
				return err
			}

			bootstrap, err := inv.Bootstrap()
			if err != nil {
				return err
			}

			a.log.Info("source    %s", inv.Source)
			a.log.Info("topology  %s", inv.Summary())
			a.log.Info("")

			rows := make([][]string, 0, len(inv.Servers))
			for _, s := range inv.Servers {
				marker := ""
				if s.Host == bootstrap.Host {
					marker = "*"
				}
				rows = append(rows, []string{
					marker,
					s.Host,
					string(s.Role),
					orDash(s.PrivateIP),
					orDash(s.Name),
					labelSummary(s),
				})
			}
			a.log.Table([]string{"", "host", "role", "private ip", "node name", "labels"}, rows)
			a.log.Info("")
			a.log.Detail("* initializes the cluster")

			if w := inv.QuorumWarning(); w != "" {
				a.log.Warn("%s", w)
			}
			return nil
		},
	}
	return cmd
}

// labelSummary renders a server's labels compactly.
func labelSummary(s inventory.Server) string {
	if len(s.Labels) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(s.Labels))
	for k, v := range s.Labels {
		parts = append(parts, k+"="+v)
	}
	sortStrings(parts)
	return strings.Join(parts, ",")
}

// sortStrings sorts in place; small slices, so insertion sort is fine and keeps
// the import list minimal.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// newClusterResetCmd uninstalls Kubernetes from the servers.
func newClusterResetCmd(a *App) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Uninstall Kubernetes from every server (destructive)",
		Long: `Remove the Kubernetes distribution from every server in the inventory.

This destroys the cluster and everything running on it, including any data in
persistent volumes backed by local storage. The servers themselves are left
running — buidl does not manage infrastructure, so it will not delete machines.

Workers are removed first and the bootstrap control plane last.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := a.context()
			defer cancel()
			cmd.SetContext(ctx)

			mgr, err := a.clusterManager(cmd)
			if err != nil {
				return err
			}
			defer mgr.Close()

			inv, err := a.cfg.Infra.InventoryProvider().Resolve(ctx)
			if err != nil {
				return err
			}

			// Destroying a cluster is irreversible, so require an explicit,
			// unambiguous confirmation rather than a bare y/N.
			if !yes {
				if a.log.CI().Detected || a.log.Mode() != "pretty" {
					return fmt.Errorf("`cluster reset` destroys the cluster; pass --yes to confirm")
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nThis DESTROYS the %s cluster on %d server(s) and all data on it.\n"+
						"Servers: %s\n\nType the environment name (%q) to confirm: ",
					a.cfg.Environment, len(inv.Servers), strings.Join(inv.Hosts(), ", "), a.cfg.Environment)

				var answer string
				if _, err := fmt.Fscanln(cmd.InOrStdin(), &answer); err != nil {
					return fmt.Errorf("cancelled")
				}
				if answer != a.cfg.Environment {
					return fmt.Errorf("cancelled")
				}
			}

			if err := mgr.Reset(ctx); err != nil {
				return err
			}

			a.log.EndStep()
			a.log.Success("cluster removed from %d server(s)", len(inv.Servers))
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}
