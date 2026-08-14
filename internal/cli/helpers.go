package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danecwalker/buidl/internal/build"
	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/hooks"
	"github.com/danecwalker/buidl/internal/release"
	"github.com/danecwalker/buidl/internal/secrets"
	"github.com/danecwalker/buidl/internal/ui"
)

// secretOptions builds the resolution options for the loaded config.
func (a *App) secretOptions() secrets.Options {
	return secrets.Options{
		Root:        a.root,
		Environment: a.cfg.Environment,
		Names:       a.cfg.SecretNames(),
		Dotenv:      a.cfg.Env.Dotenv,
		DotenvFiles: a.cfg.Env.DotenvFiles,
	}
}

// resolveSecrets loads declared secret values — the app's env.secret plus
// each accessory's env.secret — and fails with a clear list when any are
// missing.
func (a *App) resolveSecrets() (map[string]string, error) {
	res, err := secrets.Resolve(a.secretOptions())
	if err != nil {
		return nil, err
	}
	a.applyAccessoryURLs(res)

	for _, w := range res.Warnings {
		a.log.Warn("%s", w)
	}

	if len(res.Missing) > 0 {
		return nil, fmt.Errorf(
			"missing required secret(s): %s\n\n"+
				"Provide a value by exporting it, or by adding it to one of:\n"+
				"  %s   (this environment only, gitignored)\n"+
				"  %s   (all environments, gitignored)\n"+
				"  %s   (committed; indirections only, e.g. %s=$PROD_%s)\n\n"+
				"Run `buidl variable list` to see where each secret resolves from.",
			strings.Join(res.Missing, ", "),
			secrets.EnvironmentFile(a.cfg.Environment),
			secrets.DefaultFile,
			secrets.CommonPath(),
			res.Missing[0], res.Missing[0])
	}

	if len(res.Files) > 0 {
		a.log.Detail("secrets from %s", strings.Join(res.Files, ", "))
	}
	// Report where values came from, never the values themselves.
	for name, src := range res.Sources {
		a.log.Detail("secret %s from %s", name, src)
	}
	// A variable that exists locally but is not declared will not be in the
	// cluster; saying so prevents a confusing runtime failure.
	if len(res.Discovered) > 0 {
		a.log.Detail("not deployed (undeclared in env.secret): %s", strings.Join(res.Discovered, ", "))
	}
	return res.Values, nil
}

// applyAccessoryURLs fills declared connection URLs that can be derived from
// typed accessories, so a Postgres accessory does not also require the user
// to type DATABASE_URL by hand.
func (a *App) applyAccessoryURLs(res *secrets.Resolution) {
	if a.cfg == nil || res == nil {
		return
	}
	for name, value := range config.SynthesizeAccessoryURLs(a.cfg, res.Values) {
		res.Values[name] = value
		res.Sources[name] = secrets.SourceDerived
		res.Missing = removeString(res.Missing, name)
	}
}

func removeString(list []string, want string) []string {
	out := list[:0]
	for _, s := range list {
		if s != want {
			out = append(out, s)
		}
	}
	return out
}

// openConfigFile finds buidl.yaml and opens it as a comment-preserving document.
// Used by commands that write the file rather than resolve an environment.
func (a *App) openConfigFile() (*config.File, error) {
	path, err := config.ResolvePath(config.LoadOptions{Path: a.opts.configPath})
	if err != nil {
		return nil, err
	}
	f, err := config.Open(path)
	if err != nil {
		return nil, err
	}
	a.path = path
	a.root = filepath.Dir(path)
	return f, nil
}

// validateEditedConfig loads every declared environment so a write cannot
// leave a file the rest of the tool cannot parse.
func (a *App) validateEditedConfig(f *config.File, extra string) error {
	names := f.EnvironmentNames()
	if extra != "" {
		found := false
		for _, n := range names {
			if n == extra {
				found = true
				break
			}
		}
		if !found {
			names = append(names, extra)
		}
	}
	if len(names) == 0 {
		_, err := config.Load(config.LoadOptions{Path: f.Path, Strict: true, Vars: map[string]string{"BUIDL_SLUG": "example"}})
		return err
	}
	for _, name := range names {
		if _, err := config.Load(config.LoadOptions{
			Path:        f.Path,
			Environment: name,
			Strict:      true,
			Vars:        map[string]string{"BUIDL_SLUG": "example"},
		}); err != nil {
			return fmt.Errorf("the edited config did not validate for environment %q: %w", name, err)
		}
	}
	return nil
}

// hookRunner builds the lifecycle hook runner for the loaded config.
func (a *App) hookRunner() *hooks.Runner {
	return hooks.NewRunner(a.root, a.cfg.HooksPath, a.log)
}

// hookContext assembles the values hooks receive in their environment.
func (a *App) hookContext(rel release.Release, secretValues map[string]string, url string) hooks.Context {
	return hooks.Context{
		App:         a.cfg.App,
		Environment: a.cfg.Environment,
		Release:     rel.ID,
		Digest:      rel.Digest,
		Image:       rel.Ref(),
		Namespace:   a.cfg.Deploy.Kubernetes.Namespace,
		GitSHA:      a.git.SHA,
		GitBranch:   a.git.Branch,
		Actor:       rel.Actor,
		URL:         url,
		Version:     Version,
		Secrets:     secretValues,
	}
}

// runFailureHook fires the deploy-failed hook, swallowing its own error.
//
// The deploy has already failed; a broken notification hook must not replace the
// real cause with a less useful one.
func (a *App) runFailureHook(ctx context.Context, hookCtx hooks.Context) {
	if err := a.runHook(ctx, hooks.DeployFailed, hookCtx); err != nil {
		a.log.Detail("deploy-failed hook: %v", err)
	}
}

// runHook executes a lifecycle hook, returning an error only when the hook's
// failure should stop the deploy.
func (a *App) runHook(ctx context.Context, point hooks.Point, hookCtx hooks.Context) error {
	result := a.hookRunner().Run(ctx, point, hookCtx)
	if !result.Ran {
		return nil
	}
	if result.Err == nil {
		a.log.Success("%s hook completed in %s", point, result.Duration.Round(time.Millisecond))
		return nil
	}
	if point.Aborts() {
		return result.Err
	}
	// A post-deploy or notification hook cannot undo a successful deploy, so its
	// failure is reported without changing the outcome.
	a.log.Warn("%v", result.Err)
	return nil
}

// buildRelease builds (or resolves) the image and returns a digest-pinned
// release.
func (a *App) buildRelease(ctx context.Context, rel release.Release, push, noCache bool, platforms []string) (release.Release, error) {
	builder, err := build.For(a.cfg, a.log)
	if err != nil {
		return rel, err
	}
	defer builder.Close()

	if err := builder.Available(ctx); err != nil {
		return rel, err
	}

	a.log.Step(fmt.Sprintf("Building %s", rel.TagRef()))

	result, err := builder.Build(ctx, build.Request{
		Root:      a.root,
		Config:    a.cfg,
		Release:   rel,
		Push:      push,
		NoCache:   noCache,
		Platforms: platforms,
		// The interactive BuildKit display only makes sense on a terminal.
		Plain: a.log.Mode() != ui.ModePretty,
	})
	if err != nil {
		return rel, err
	}

	rel.Digest = result.Digest
	a.log.StepDetail("%s%s", rel.ShortDigest(), platformNote(result.Platforms))
	a.log.Success("built %s in %s", rel.ShortDigest(), result.Duration.Round(time.Millisecond))
	return rel, nil
}

// platformNote annotates a multi-arch build, which is worth surfacing because it
// roughly doubles build time.
func platformNote(platforms []string) string {
	if len(platforms) < 2 {
		return ""
	}
	return fmt.Sprintf(" (%s)", strings.Join(platforms, ", "))
}

// renderPlan prints an application plan.
//
// The goal is that a reviewer can answer "what will this do" from the table
// alone: which objects, which fields, and what the runtime consequence is. The
// raw YAML diff stays available behind --detailed for when that is not enough.
func (a *App) renderPlan(plan *deploy.Plan, detailed bool) {
	a.log.EndStep()

	for _, w := range plan.Warnings {
		a.log.Warn("%s", w)
	}

	a.log.KeyValues([][2]string{
		{"environment", plan.Environment},
		{"release", plan.Release.ID},
		{"image", plan.Release.ShortDigest()},
	})
	a.log.Info("")

	if !plan.HasChanges() {
		a.log.Success("no changes; %s is already up to date", plan.Environment)
		return
	}

	// Unchanged objects are listed too, at low prominence, so the plan accounts
	// for every object buidl manages rather than only the deltas.
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

	if detailed {
		for _, c := range plan.Changes {
			if c.Diff == "" {
				continue
			}
			a.log.Info("")
			a.log.Info("--- %s/%s", c.Kind, c.Name)
			a.log.Indented("  ", c.Diff)
		}
	}

	counts := plan.Counts()
	a.log.Info("")
	a.log.Info("plan: %d to create, %d to update, %d unchanged",
		counts[deploy.ActionCreate], counts[deploy.ActionUpdate], counts[deploy.ActionUnchanged])
	if !detailed {
		a.log.Detail("pass --detailed for full object diffs")
	}
}

// actionMarker renders an action as a compact diff-style marker.
func actionMarker(action deploy.Action) string {
	switch action {
	case deploy.ActionCreate:
		return "+"
	case deploy.ActionDelete:
		return "-"
	case deploy.ActionUpdate:
		return "~"
	default:
		return " "
	}
}

// reportOutcome prints a deploy result and exposes values to CI.
//
// A successful deploy ends with three things: what changed, what is running now,
// and where the time went. Reporting only "success" leaves every follow-up
// question ("did it restart? is it actually serving?") unanswered.
func (a *App) reportOutcome(outcome *deploy.Outcome) {
	a.log.EndStep()

	a.reportChanges(outcome)
	a.reportInstances(outcome)

	a.log.Summary("Deploy summary")

	if outcome.RolledBack {
		a.log.Warn("rolled back to %s", outcome.PreviousRelease)
	} else {
		a.log.Success("deployed %s in %s", outcome.Release.ID, outcome.Duration.Round(time.Second))
	}

	a.log.KeyValues([][2]string{
		{"release", outcome.Release.ID},
		{"image", outcome.Release.ShortDigest()},
		{"previous", outcome.PreviousRelease},
		{"url", outcome.URL},
	})

	if a.log.Mode() == ui.ModeJSON {
		fields := map[string]any{
			"release":   outcome.Release.ID,
			"digest":    outcome.Release.Digest,
			"duration":  outcome.Duration.Round(time.Second).String(),
			"applied":   len(outcome.Applied()),
			"instances": len(outcome.Instances),
		}
		if outcome.URL != "" {
			fields["url"] = outcome.URL
		}
		if outcome.PreviousRelease != "" {
			fields["previous"] = outcome.PreviousRelease
		}
		if outcome.RolledBack {
			fields["rolled_back"] = true
		}
		a.log.Fields("deploy complete", fields)
	}

	a.exportToCI(map[string]string{
		"release": outcome.Release.ID,
		"digest":  outcome.Release.Digest,
		"url":     outcome.URL,
	})
}

// reportChanges lists what the deploy actually did to the cluster.
func (a *App) reportChanges(outcome *deploy.Outcome) {
	if len(outcome.Changes) == 0 {
		return
	}

	rows := make([][]string, 0, len(outcome.Changes))
	for _, c := range outcome.Changes {
		status := "applied"
		switch {
		case c.Err != nil:
			status = "FAILED"
		case !c.Applied:
			status = "not applied"
		case c.Action == deploy.ActionUnchanged:
			status = "unchanged"
		}
		rows = append(rows, []string{
			status,
			c.Kind,
			c.Name,
			c.FieldSummary(),
			c.Impact,
		})
	}

	a.log.Info("")
	a.log.Info("Changes")
	a.log.Table([]string{"status", "kind", "name", "changes", "effect"}, rows)
}

// reportInstances lists the running instances after a deploy.
func (a *App) reportInstances(outcome *deploy.Outcome) {
	if len(outcome.Instances) == 0 {
		return
	}

	ready := 0
	rows := make([][]string, 0, len(outcome.Instances))
	for _, p := range outcome.Instances {
		if p.Ready {
			ready++
		}
		rows = append(rows, []string{
			p.Name,
			p.Phase,
			yesNo(p.Ready),
			fmt.Sprintf("%d", p.Restarts),
			humanAge(p.Age),
			orDash(p.Node),
			truncate(p.Message, 30),
		})
	}

	a.log.Info("")
	a.log.Info("Running instances (%d/%d ready)", ready, len(outcome.Instances))
	a.log.Table([]string{"instance", "phase", "ready", "restarts", "age", "node", "message"}, rows)
}

// reportPartialFailure explains a deploy that failed partway through applying.
//
// Apply is not atomic, so this is the difference between a user knowing the
// cluster is in a mixed state and having to reverse-engineer it with kubectl.
func (a *App) reportPartialFailure(outcome *deploy.Outcome) {
	if outcome == nil || !outcome.Partial {
		return
	}

	a.log.Warn("the deploy failed partway through; the namespace is in a mixed state")

	applied := outcome.Applied()
	if len(applied) > 0 {
		items := make([]string, 0, len(applied))
		for _, c := range applied {
			items = append(items, fmt.Sprintf("%s/%s", c.Kind, c.Name))
		}
		a.log.Bullets("already applied", items)
	}

	for _, c := range outcome.Failed() {
		a.log.Error("failed to apply %s/%s: %v", c.Kind, c.Name, c.Err)
	}

	a.log.Info("")
	a.log.Info("re-run `buidl deploy` once the cause is fixed; apply is idempotent,")
	a.log.Info("or inspect the current state with `buidl status`")
}

// exportToCI writes step outputs so a workflow can consume the release ID and
// URL — for example, to post a preview link on a pull request.
func (a *App) exportToCI(values map[string]string) {
	ci := a.log.CI()
	path := ci.OutputFile()
	if path == "" {
		return
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		a.log.Detail("could not write CI outputs: %v", err)
		return
	}
	defer f.Close()

	for k, v := range values {
		if v == "" {
			continue
		}
		line := ci.SetOutput(k, v)
		if line == "" {
			continue
		}
		if _, err := fmt.Fprintln(f, line); err != nil {
			a.log.Detail("could not write CI output %s: %v", k, err)
			return
		}
	}
}

// deployRequest assembles a deploy.Request from loaded state.
func (a *App) deployRequest(rel release.Release, secretValues map[string]string, wait, autoRollback bool) deploy.Request {
	return deploy.Request{
		Config:       a.cfg,
		Release:      rel,
		Root:         a.root,
		Secrets:      secretValues,
		Wait:         wait,
		AutoRollback: autoRollback,
	}
}

// humanAge renders a duration compactly, as a status table needs.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// yesNo renders a boolean for a table cell.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// confirm asks a yes/no question and turns anything but an explicit yes into
// the supplied cancellation error.
//
// Callers are responsible for deciding whether a prompt is warranted at all —
// this is only reached once they have. A read error means there is no usable
// stdin, which is treated as declining: a destructive command must never
// proceed because nobody was there to say no.
func (a *App) confirm(cmd *cobra.Command, question, cancelled string) error {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)

	var answer string
	if _, err := fmt.Fscanln(cmd.InOrStdin(), &answer); err != nil {
		return fmt.Errorf("%s", cancelled)
	}
	switch answer {
	case "y", "Y", "yes", "Yes":
		return nil
	default:
		return fmt.Errorf("%s", cancelled)
	}
}
