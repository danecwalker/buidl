package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy/kubernetes"
	"github.com/danecwalker/buidl/internal/ui"
	"github.com/danecwalker/buidl/internal/watch"
)

// newWatchCmd is the live operational dashboard.
func newWatchCmd(a *App) *cobra.Command {
	var (
		once     bool
		interval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "watch [APP]",
		Short: "Live dashboard of health, RAM, CPU, and uptime",
		Long: `Watch the stack the way you would a status page.

A boxed live dashboard: stack and cluster cards, per-app CPU/RAM
sparklines, gauges, and the selected app's instances. Health, ready
counts, uptime, restarts, and the live release sit in the same frame.

RAM and CPU come from metrics-server. k3s bundles it unless disabled;
without it those series show — and everything else still updates.

On a terminal this is a live view (q to quit, j/k to select, r to
refresh). ` + "`--once`" + ` and non-TTY stdout print one snapshot and exit.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			live := !once && !a.log.CI().Detected &&
				term.IsTerminal(int(os.Stdin.Fd())) &&
				term.IsTerminal(int(os.Stdout.Fd()))

			var ctx context.Context
			var cancel context.CancelFunc
			if live {
				// The global default is 30m so a hung deploy cannot run
				// forever. A dashboard you leave up is the opposite: only
				// an explicit --timeout should bound it. Ctrl+C is quit,
				// not the two-press confirm used during apply.
				ctx, cancel = a.watchContext(cmd)
			} else {
				ctx, cancel = a.context()
			}
			defer cancel()
			cmd.SetContext(ctx)

			if err := a.requireConfig(ctx); err != nil {
				return err
			}
			if err := a.ensureClusterCredentials(cmd); err != nil {
				return err
			}
			if name != "" && a.cfg.Member(name) == config.MemberNone {
				return a.cfg.UnknownAppError(name)
			}

			target, err := a.target()
			if err != nil {
				return err
			}
			defer target.Close()

			kt, ok := target.(*kubernetes.Target)
			if !ok {
				return fmt.Errorf("watch is only supported for the kubernetes target")
			}

			collect := func(ctx context.Context) (watch.Snapshot, error) {
				return kt.WatchSnapshot(ctx, a.cfg)
			}

			if !live {
				snap, err := collect(ctx)
				if err != nil {
					return err
				}
				return a.printWatch(cmd, snap, name)
			}

			color := a.log.Mode() == ui.ModePretty && !a.opts.noColor
			return watch.Live(ctx, watch.Options{
				Collect:  collect,
				Interval: interval,
				Color:    color,
				Select:   name,
			})
		},
	}

	cmd.Flags().BoolVar(&once, "once", false, "print one snapshot and exit")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval")
	return cmd
}

func (a *App) printWatch(cmd *cobra.Command, snap watch.Snapshot, name string) error {
	if a.log.Mode() == ui.ModeJSON {
		fields, err := snapshotFields(snap)
		if err != nil {
			return err
		}
		a.log.Fields("watch", fields)
		return nil
	}

	width := 100
	if fd := int(os.Stdout.Fd()); term.IsTerminal(fd) {
		if w, _, err := term.GetSize(fd); err == nil && w > 0 {
			width = w
		}
	}
	color := a.log.Mode() == ui.ModePretty && !a.opts.noColor
	// Highlight the named app in the one-shot table the same way the TUI would.
	view := watch.View{
		Snapshot:    snap,
		Selected:    snap.SelectIndex(name),
		Now:         time.Now(),
		Width:       width,
		Color:       color,
		Interactive: false,
	}
	fmt.Fprint(cmd.OutOrStdout(), watch.Render(view)+"\n")
	return nil
}

func snapshotFields(snap watch.Snapshot) (map[string]any, error) {
	// Re-encode so durations become the same human strings as the table.
	apps := make([]map[string]any, 0, len(snap.Apps))
	for _, app := range snap.Apps {
		instances := make([]map[string]any, 0, len(app.Instances))
		for _, inst := range app.Instances {
			instances = append(instances, map[string]any{
				"name":     inst.Name,
				"phase":    inst.Phase,
				"ready":    inst.Ready,
				"restarts": inst.Restarts,
				"cpu":      watch.FormatCPU(inst.Usage),
				"memory":   watch.FormatMemory(inst.Usage),
				"uptime":   watch.FormatUptime(inst.StartedAt, inst.Uptime),
				"node":     inst.Node,
				"release":  inst.Release,
				"message":  inst.Message,
			})
		}
		apps = append(apps, map[string]any{
			"name":      app.Name,
			"type":      app.Type,
			"health":    app.Health,
			"ready":     app.Ready,
			"desired":   app.Desired,
			"cpu":       watch.FormatCPU(app.Usage),
			"memory":    watch.FormatMemory(app.Usage),
			"uptime":    watch.FormatUptime(app.StartedAt, app.Uptime),
			"restarts":  app.Restarts,
			"release":   app.Release,
			"url":       app.URL,
			"instances": instances,
		})
	}
	nodes := make([]map[string]any, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		nodes = append(nodes, map[string]any{
			"name":   n.Name,
			"ready":  n.Ready,
			"cpu":    watch.FormatNodeCPU(n),
			"memory": watch.FormatNodeMemory(n),
			"uptime": watch.FormatAge(n.Age),
			"roles":  n.Roles,
		})
	}
	raw := map[string]any{
		"stack":       snap.Stack,
		"environment": snap.Environment,
		"namespace":   snap.Namespace,
		"context":     snap.Context,
		"metrics":     string(snap.Metrics),
		"apps":        apps,
		"nodes":       nodes,
		"alerts":      snap.Alerts,
	}
	// Force the same types Fields would JSON-encode, so tests can round-trip.
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// watchContext is a session that lasts until cancel, SIGTERM, or an explicit
// --timeout. The default 30m command timeout does not apply.
func (a *App) watchContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	parent := context.Background()
	stopTimeout := func() {}
	if cmd.InheritedFlags().Changed("timeout") {
		parent, stopTimeout = context.WithTimeout(parent, a.opts.timeout)
	}
	ctx, cancel := context.WithCancel(parent)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
		case <-sigs:
			cancel()
		}
	}()
	return ctx, func() {
		signal.Stop(sigs)
		cancel()
		stopTimeout()
	}
}
