package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/danecwalker/buidl/internal/config"
)

// newEnvironmentCmd manages the named overlays in buidl.yaml.
func newEnvironmentCmd(a *App) *cobra.Command {
	list := newEnvironmentListCmd(a)
	cmd := &cobra.Command{
		Use:          "environment",
		Aliases:      []string{"env", "environments"},
		Hidden:       true,
		SilenceUsage: true,
		Short:        "Manage deployment environments",
		Long: `Create, list, and remove environment overlays in buidl.yaml.

An environment is an opt-in overlay. ` + "`buidl init`" + ` writes none; a
single-target app deploys with no ` + "`-e`" + `. Add staging, production, or
preview when you actually want more than one target.

The first environment you create becomes defaultEnvironment. Production is
never implied from an empty ` + "`-e`" + ` when several overlays exist and no
default is set.

  buidl environment list
  buidl environment new qa
  buidl environment new qa --from staging --host qa.example.com
  buidl environment set production
  buidl environment delete qa

This is a file edit. It does not create or destroy cluster objects — that is
` + "`buidl deploy`" + ` and ` + "`buidl destroy -e`" + `.`,
		Args: cobra.NoArgs,
		RunE: list.RunE,
	}
	cmd.AddCommand(
		list,
		newEnvironmentNewCmd(a),
		newEnvironmentSetCmd(a),
		newEnvironmentDeleteCmd(a),
	)
	return cmd
}

func newEnvironmentListCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the environments declared in buidl.yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			names := f.EnvironmentNames()
			if len(names) == 0 {
				a.log.Info("no environments declared; this config targets a single environment")
				return nil
			}

			def := f.DefaultEnvironment()
			rows := make([][]string, 0, len(names))
			for _, name := range names {
				mark := ""
				if name == def || (def == "" && strings.EqualFold(name, "staging")) {
					mark = "*"
				}
				host := f.String("environments", name, "proxy", "host")
				ns := f.String("environments", name, "deploy", "kubernetes", "namespace")
				rows = append(rows, []string{mark, name, orDash(host), orDash(ns)})
			}
			a.log.Table([]string{"", "name", "host", "namespace"}, rows)
			if def != "" {
				a.log.Detail("default environment: %s (used when -e is omitted)", def)
			}
			return nil
		},
	}
	return cmd
}

func newEnvironmentNewCmd(a *App) *cobra.Command {
	var (
		from string
		host string
	)

	cmd := &cobra.Command{
		Use:     "new NAME",
		Aliases: []string{"add", "create"},
		Short:   "Add an environment overlay to buidl.yaml",
		Long: `Write a new environment overlay.

A well-known name (staging, production, preview) uses that template. Any other
name uses the staging shape — create the namespace, stay off production
defaults. ` + "`--from`" + ` copies an existing overlay, or selects a template
when that name is not already declared.

  buidl environment new qa
  buidl environment new qa --from production --host qa.example.com
  buidl environment new preview`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.ToLower(args[0])
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			if err := a.addEnvironment(f, name, from, host); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			if err := a.validateEditedConfig(f, name); err != nil {
				return err
			}

			a.log.Success("created environment %q", name)
			if h := f.String("environments", name, "proxy", "host"); h != "" {
				a.log.Detail("host %s", h)
			}
			if ns := f.String("environments", name, "deploy", "kubernetes", "namespace"); ns != "" {
				a.log.Detail("namespace %s", ns)
			}
			a.log.Info("")
			a.log.Info("next: buidl deploy -e %s", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "copy an existing environment, or a template (staging, production, preview)")
	cmd.Flags().StringVar(&host, "host", "", "proxy hostname for this environment")
	return cmd
}

func newEnvironmentSetCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "set [NAME]",
		Aliases: []string{"use"},
		Short:   "Set the default environment used when -e is omitted",
		Long: `Write defaultEnvironment so commands without -e target this overlay.

Production is a valid default if you set it here. It is never implied.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				def := f.DefaultEnvironment()
				if def == "" {
					a.log.Info("no defaultEnvironment is set")
					return nil
				}
				a.log.Info("%s", def)
				return nil
			}

			name := args[0]
			if !containsString(f.EnvironmentNames(), name) {
				return fmt.Errorf("unknown environment %q (declared: %s)", name, strings.Join(f.EnvironmentNames(), ", "))
			}
			if err := f.SetString([]string{"defaultEnvironment"}, name); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			a.log.Success("default environment is now %s", name)
			return nil
		},
	}
	return cmd
}

func newEnvironmentDeleteCmd(a *App) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "delete NAME",
		Aliases: []string{"rm", "remove"},
		Short:   "Remove an environment overlay from buidl.yaml",
		Long: `Delete an overlay from the file. Cluster objects are not touched.

Refuses to delete the default environment without --force, so a later
` + "`buidl deploy`" + ` cannot silently retarget. After a forced delete, staging
is preferred as the new default when it remains.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			if !containsString(f.EnvironmentNames(), name) {
				return fmt.Errorf("unknown environment %q (declared: %s)", name, strings.Join(f.EnvironmentNames(), ", "))
			}

			if f.DefaultEnvironment() == name && !force {
				return fmt.Errorf("refusing to delete default environment %q\n\nhint: pass --force, or `buidl environment set` another default first", name)
			}

			f.Delete("environments", name)
			remaining := f.EnvironmentNames()
			if len(remaining) == 0 {
				f.Delete("environments")
				f.Delete("defaultEnvironment")
			} else if f.DefaultEnvironment() == name {
				if next := nextDefaultEnvironment(remaining); next != "" {
					if err := f.SetString([]string{"defaultEnvironment"}, next); err != nil {
						return err
					}
				} else {
					f.Delete("defaultEnvironment")
				}
			}
			if err := f.Save(); err != nil {
				return err
			}

			a.log.Success("removed environment %q from %s", name, a.path)
			a.log.Detail("cluster objects were not changed; run `buidl destroy -e %s` to tear them down", name)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "allow deleting the default environment")
	return cmd
}

// addEnvironment writes one overlay. The caller saves. First overlay becomes
// defaultEnvironment. Used by `environment new` and by `init` when the user
// asked for staging / review apps.
func (a *App) addEnvironment(f *config.File, name, from, host string) error {
	if !config.ValidDNSLabel(name) {
		return fmt.Errorf("environment name %q must be a lowercase DNS label", name)
	}
	if containsString(f.EnvironmentNames(), name) {
		return fmt.Errorf("environment %q already exists\n\nhint: `buidl environment list` shows what is declared", name)
	}
	appName := f.App()
	if appName == "" {
		return fmt.Errorf("%s has no `app`", f.Path)
	}
	overlay, err := environmentOverlay(f, name, appName, from, host)
	if err != nil {
		return err
	}
	if err := f.Set([]string{"environments", name}, overlay); err != nil {
		return err
	}
	if f.DefaultEnvironment() == "" {
		if err := f.SetString([]string{"defaultEnvironment"}, name); err != nil {
			return err
		}
	}
	return nil
}

func environmentOverlay(f *config.File, name, app, from, host string) (*yaml.Node, error) {
	if from != "" {
		if src := f.Lookup("environments", from); src != nil && !isYAMLNull(src) {
			cloned := config.CloneNode(src)
			if host != "" {
				if err := setNodeString(cloned, []string{"proxy", "host"}, host); err != nil {
					return nil, err
				}
			}
			return cloned, nil
		}
		if _, ok := config.ParseEnvironmentKind(from); !ok {
			return nil, fmt.Errorf("unknown --from %q (declared: %s; templates: staging, production, preview)",
				from, strings.Join(f.EnvironmentNames(), ", "))
		}
	}

	kind := config.InferEnvironmentKind(name, from)
	node, err := config.OverlayNode(kind, name, app, host)
	if err != nil {
		return nil, err
	}
	return node, nil
}

func isYAMLNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "" || n.Value == "null" || n.Value == "~")
}

func setNodeString(root *yaml.Node, path []string, value string) error {
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("cannot set %s on a non-mapping", strings.Join(path, "."))
	}
	n := root
	for i, key := range path {
		if i == len(path)-1 {
			found := false
			for j := 0; j < len(n.Content)-1; j += 2 {
				if n.Content[j].Value == key {
					n.Content[j+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
					found = true
					break
				}
			}
			if !found {
				n.Content = append(n.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
				)
			}
			return nil
		}
		var child *yaml.Node
		for j := 0; j < len(n.Content)-1; j += 2 {
			if n.Content[j].Value == key {
				child = n.Content[j+1]
				break
			}
		}
		if child == nil || (child.Kind == yaml.ScalarNode && child.Value == "") {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
				child,
			)
		}
		if child.Kind != yaml.MappingNode {
			return fmt.Errorf("%s is not a mapping", strings.Join(path[:i+1], "."))
		}
		n = child
	}
	return nil
}

// nextDefaultEnvironment prefers staging so a forced delete of production
// cannot leave production implied.
func nextDefaultEnvironment(names []string) string {
	for _, n := range names {
		if strings.EqualFold(n, "staging") {
			return n
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	return ""
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
