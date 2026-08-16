package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/inventory"
	"github.com/danecwalker/buidl/internal/secrets"
)

// newAddCmd writes a server, domain, or app into buidl.yaml.
func newAddCmd(a *App) *cobra.Command {
	var (
		database string
		service  bool
		name     string
		host     string
		path     string
		disk     string
		image    string
		port     int
		command  []string
	)

	cmd := &cobra.Command{
		Use:          "add",
		SilenceUsage: true,
		Short:        "Add a server, domain, or app to the stack",
		Long: `Grow the stack. A server is a machine. A domain is a hostname on an app.
Everything else is an app: postgres, redis, an api, a worker.

  buidl add server 203.0.113.10 --email you@example.com
  buidl add domain example.com
  buidl add postgres
  buidl add api --image ghcr.io/acme/api --host api.example.com
  buidl add worker --command ./worker

A second domain without --app is an extra hostname on the first app (www).
A separate process is ` + "`buidl add api`" + `.

Postgres and Redis are created on first deploy if they are missing. Later
deploys leave them alone. Reconcile one with ` + "`buidl deploy postgres`" + `.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if database != "" && service {
				return fmt.Errorf("pass --database or --service, not both")
			}
			if len(args) == 1 && name == "" {
				name = args[0]
			}

			f, err := a.openConfigFile()
			if err != nil {
				return err
			}

			if service {
				return a.addService(f, name, host, path)
			}
			if database != "" {
				return a.addDatabase(f, database, name, disk)
			}
			if name != "" {
				return a.addStackMember(f, name, host, path, image, port, command)
			}
			return cmd.Help()
		},
	}

	f := cmd.Flags()
	f.StringVar(&database, "database", "", "add a typed stateful app: postgres or redis")
	f.BoolVar(&service, "service", false, "configure this app's host or health path")
	f.StringVar(&name, "name", "", "stateful app name (default: the type)")
	f.StringVar(&host, "host", "", "proxy hostname for this app")
	f.StringVar(&path, "path", "", "healthcheck path")
	f.StringVar(&disk, "disk", "", "persistent volume size (e.g. 20Gi)")
	f.StringVar(&image, "image", "", "image repository")
	f.IntVar(&port, "port", 0, "container port")
	f.StringSliceVar(&command, "command", nil, "container command (worker)")
	_ = f.MarkHidden("database")
	_ = f.MarkHidden("service")
	_ = f.MarkHidden("name")
	_ = f.MarkHidden("disk")

	cmd.AddCommand(
		newAddServerCmd(a),
		newAddDomainCmd(a),
		newAddPostgresCmd(a),
		newAddRedisCmd(a),
		newAddAppCmd(a),
	)
	return cmd
}

func newAddServerCmd(a *App) *cobra.Command {
	var (
		user  string
		port  int
		role  string
		email string
	)

	cmd := &cobra.Command{
		Use:   "server HOST",
		Short: "Add a machine to the fleet",
		Long: `Write a server into infra.servers. buidl never creates the VM —
bring it up yourself, then point this command at it.

  buidl add server 203.0.113.10 --email you@example.com
  buidl add server 203.0.113.11 --role worker

The first server becomes the control plane. --email is the Let's Encrypt
contact and is required once a domain (TLS) is configured.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			return a.addServer(f, args[0], user, port, role, email)
		},
	}

	cmd.Flags().StringVar(&user, "user", "", "SSH user (default: root)")
	cmd.Flags().IntVar(&port, "port", 0, "SSH port (default: 22)")
	cmd.Flags().StringVar(&role, "role", "", "control-plane or worker (default: first server is control-plane)")
	cmd.Flags().StringVar(&email, "email", "", "Let's Encrypt contact (infra.addons.certManagerEmail)")
	return cmd
}

func newAddDomainCmd(a *App) *cobra.Command {
	var email, appName string

	cmd := &cobra.Command{
		Use:   "domain HOST",
		Short: "Add a hostname an app should serve",
		Long: `Write a hostname into the proxy. The first call is the primary
host. Later calls (www, …) are aliases on the same app: one Ingress, one
certificate, every name on the same Service.

  buidl add domain example.com --email you@example.com
  buidl add domain www.example.com
  buidl add domain api.example.com --app api

A separate API process is ` + "`buidl add api --host api.example.com`" + `,
not a second domain on the first app.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			return a.addDomain(f, args[0], email, appName)
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "Let's Encrypt contact (infra.addons.certManagerEmail)")
	cmd.Flags().StringVar(&appName, "app", "", "app that should serve this hostname (default: the first app)")
	return cmd
}

func newAddPostgresCmd(a *App) *cobra.Command {
	return newAddTypedDatabaseCmd(a, "postgres")
}

func newAddRedisCmd(a *App) *cobra.Command {
	return newAddTypedDatabaseCmd(a, "redis")
}

func newAddTypedDatabaseCmd(a *App, kind string) *cobra.Command {
	var (
		name string
		disk string
	)

	cmd := &cobra.Command{
		Use:   kind,
		Short: "Add a " + kind + " app",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			return a.addDatabase(f, kind, name, disk)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "app name (default: "+kind+")")
	cmd.Flags().StringVar(&disk, "disk", "", "persistent volume size (e.g. 20Gi)")
	return cmd
}

func newAddAppCmd(a *App) *cobra.Command {
	var (
		host  string
		path  string
		image string
	)

	cmd := &cobra.Command{
		Use:    "app [NAME]",
		Short:  "Add or configure a process app",
		Hidden: true,
		Long: `Hidden alias of ` + "`buidl add NAME`" + `.

  buidl add api --image ghcr.io/acme/api --host api.example.com`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			if name == "" {
				return a.addApp(f, "", host, path, image)
			}
			return a.addStackMember(f, name, host, path, image, 0, nil)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "proxy hostname for this app")
	cmd.Flags().StringVar(&path, "path", "", "healthcheck path")
	cmd.Flags().StringVar(&image, "image", "", "image repository")
	return cmd
}

func (a *App) addServer(f *config.File, host, user string, port int, role, email string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("server host is required")
	}
	if strings.ContainsAny(host, "/@") {
		return fmt.Errorf("host must be a hostname or IP without scheme or user (got %q)", host)
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		if port != 0 {
			return fmt.Errorf("pass the port with --port, not in the host")
		}
		host = h
		port, err = strconv.Atoi(p)
		if err != nil {
			return fmt.Errorf("invalid port in %q", host)
		}
	}
	if role != "" && !inventory.Role(role).Valid() {
		return fmt.Errorf("role must be control-plane or worker (got %q)", role)
	}
	if port != 0 && (port < 1 || port > 65535) {
		return fmt.Errorf("port must be between 1 and 65535 (got %d)", port)
	}

	for _, existing := range serverHosts(f) {
		if existing == host {
			return fmt.Errorf("server %q is already listed\n\nhint: this command will not update an existing server", host)
		}
	}

	if err := a.ensureInfra(f, user, port, email); err != nil {
		return err
	}
	if proxyWantsTLS(f) && f.String("infra", "addons", "certManagerEmail") == "" {
		return fmt.Errorf("a domain is configured, so TLS needs a Let's Encrypt contact\n\nhint: buidl add server %s --email you@example.com", host)
	}

	raw := "host: " + host + "\n"
	if role != "" {
		raw += "role: " + role + "\n"
	}
	node, err := config.ParseNode(raw)
	if err != nil {
		return err
	}
	if err := f.Append([]string{"infra", "servers"}, node); err != nil {
		return err
	}
	if err := f.Save(); err != nil {
		return err
	}
	if err := a.validateEditedConfig(f, ""); err != nil {
		return err
	}

	a.log.Success("added server %s", host)
	if role != "" {
		a.log.Detail("role %s", role)
	}
	if email := f.String("infra", "addons", "certManagerEmail"); email != "" {
		a.log.Detail("cert-manager %s", email)
	}
	a.log.Info("if SSH fails on an unknown host key: ssh-keyscan -H %s >> ~/.ssh/known_hosts", host)
	a.log.Info("")
	a.log.Info("next: buidl add domain <hostname>   # optional")
	a.log.Info("      buidl deploy")
	return nil
}

func (a *App) addDomain(f *config.File, host, email, appName string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("domain is required")
	}
	if strings.ContainsAny(host, "/:") {
		return fmt.Errorf("host must be a bare hostname without scheme or port (got %q)", host)
	}

	if appName != "" && appName != f.App() && containsString(f.ExtraAppNames(), appName) {
		return a.addDomainToApp(f, appName, host, email)
	}
	if appName != "" && appName != f.App() {
		return fmt.Errorf("unknown app %q (first app is %q; extra: %s)",
			appName, f.App(), strings.Join(orDashList(f.ExtraAppNames()), ", "))
	}

	prefix := a.proxyPrefix(f)
	primary := f.String(append(prefix, "host")...)
	aliases := f.Strings(append(prefix, "hosts")...)
	if host == primary || containsString(aliases, host) {
		return fmt.Errorf("domain %q is already configured", host)
	}

	if err := a.setCertEmail(f, email); err != nil {
		return err
	}
	if f.Lookup("infra") != nil && f.String("infra", "addons", "certManagerEmail") == "" {
		return fmt.Errorf("this stack has servers; TLS needs a Let's Encrypt contact\n\nhint: buidl add domain %s --email you@example.com", host)
	}

	// A first real hostname fills template overlay hosts (staging.example.com
	// and friends) so init --staging then add domain never needs a YAML edit.
	// A name under an existing host is an alias, even when that host is still
	// a template (api.staging.example.com on staging.example.com).
	replaceTemplates := isTemplateHost(primary) && !isSubdomainOrEqual(host, primary) && !hasRealProxyHost(f)
	isPrimary := primary == "" || replaceTemplates
	if replaceTemplates {
		if err := syncEnvironmentHosts(f, host); err != nil {
			return err
		}
		// Overlays now have derived hosts. Only write this prefix if it is
		// still empty or a leftover template (no matching overlay).
		if cur := f.String(append(prefix, "host")...); cur == "" || isTemplateHost(cur) {
			if err := f.SetString(append(prefix, "host"), host); err != nil {
				return err
			}
		}
	} else if primary == "" {
		if err := f.SetString(append(prefix, "host"), host); err != nil {
			return err
		}
	} else {
		if err := f.AppendUnique(append(prefix, "hosts"), host); err != nil {
			return err
		}
	}
	if err := f.SetBool(append(prefix, "ssl"), true); err != nil {
		return err
	}
	if err := f.Save(); err != nil {
		return err
	}
	if err := a.validateEditedConfig(f, ""); err != nil {
		return err
	}

	if isPrimary {
		a.log.Success("added domain %s", host)
	} else {
		a.log.Success("added alias %s (with %s)", host, primary)
	}
	a.log.Detail("tls on")
	return nil
}

func orDashList(names []string) []string {
	if len(names) == 0 {
		return []string{"(none)"}
	}
	return names
}

func (a *App) addDomainToApp(f *config.File, appName, host, email string) error {
	if err := a.setCertEmail(f, email); err != nil {
		return err
	}
	prefix := []string{"apps", appName, "proxy"}
	primary := f.String(append(prefix, "host")...)
	aliases := f.Strings(append(prefix, "hosts")...)
	if host == primary || containsString(aliases, host) {
		return fmt.Errorf("domain %q is already configured on %s", host, appName)
	}
	if primary == "" || isTemplateHost(primary) {
		if err := f.SetString(append(prefix, "host"), host); err != nil {
			return err
		}
	} else {
		if err := f.AppendUnique(append(prefix, "hosts"), host); err != nil {
			return err
		}
	}
	if err := f.SetBool(append(prefix, "ssl"), true); err != nil {
		return err
	}
	if err := f.Save(); err != nil {
		return err
	}
	if err := a.validateEditedConfig(f, ""); err != nil {
		return err
	}
	if primary == "" || isTemplateHost(primary) {
		a.log.Success("added domain %s on %s", host, appName)
	} else {
		a.log.Success("added alias %s on %s (with %s)", host, appName, primary)
	}
	return nil
}

func (a *App) addStackMember(f *config.File, name, host, path, image string, port int, command []string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if config.SupportedAccessoryType(name) {
		return a.addDatabase(f, name, "", "")
	}
	if name == f.App() {
		return a.addApp(f, name, host, path, image)
	}
	if containsString(f.AccessoryNames(), name) {
		return fmt.Errorf("%q is already a stateful app\n\nhint: `buidl deploy %s` reconciles it", name, name)
	}
	return a.addProcessApp(f, name, host, path, image, port, command)
}

func (a *App) addProcessApp(f *config.File, name, host, path, image string, port int, command []string) error {
	if !config.ValidDNSLabel(name) {
		return fmt.Errorf("app name %q must be a lowercase DNS label", name)
	}
	if host == "" && path == "" && image == "" && port == 0 && len(command) == 0 {
		return fmt.Errorf("pass --image, --host, --port, --path, or --command to add %q", name)
	}
	if image != "" {
		if err := f.SetString([]string{"apps", name, "image"}, strings.ToLower(image)); err != nil {
			return err
		}
	}
	if port != 0 {
		if err := f.Set([]string{"apps", name, "deploy", "port"}, &yaml.Node{
			Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(port),
		}); err != nil {
			return err
		}
	}
	if path != "" {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("healthcheck path must start with / (got %q)", path)
		}
		if err := f.SetString([]string{"apps", name, "deploy", "healthcheck", "path"}, path); err != nil {
			return err
		}
	}
	if len(command) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, c := range command {
			seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: c})
		}
		if err := f.Set([]string{"apps", name, "deploy", "command"}, seq); err != nil {
			return err
		}
	}
	if host != "" {
		if strings.ContainsAny(host, "/:") {
			return fmt.Errorf("host must be a bare hostname without scheme or port (got %q)", host)
		}
		existing := f.String("apps", name, "proxy", "host")
		if existing == "" || isTemplateHost(existing) {
			if err := f.SetString([]string{"apps", name, "proxy", "host"}, host); err != nil {
				return err
			}
		} else if existing != host {
			if err := f.AppendUnique([]string{"apps", name, "proxy", "hosts"}, host); err != nil {
				return err
			}
		}
		if err := f.SetBool([]string{"apps", name, "proxy", "ssl"}, true); err != nil {
			return err
		}
	}
	if err := f.Save(); err != nil {
		return err
	}
	if err := a.validateEditedConfig(f, ""); err != nil {
		return err
	}
	a.log.Success("added app %q", name)
	if image != "" {
		a.log.Detail("image %s", strings.ToLower(image))
	}
	if host != "" {
		a.log.Detail("host %s", host)
	}
	if len(command) > 0 {
		a.log.Detail("command %s", strings.Join(command, " "))
	}
	a.log.Info("")
	a.log.Info("next: buidl deploy %s", name)
	return nil
}

func (a *App) addApp(f *config.File, name, host, path, image string) error {
	appName := f.App()
	if appName == "" {
		return fmt.Errorf("%s has no `app`", a.path)
	}
	if name != "" && name != appName {
		return a.addProcessApp(f, name, host, path, image, 0, nil)
	}
	if host == "" && path == "" && image == "" {
		return fmt.Errorf("app %q is already the service in this file\n\n"+
			"hint: pass --host, --path, or --image to configure it", appName)
	}

	if host != "" {
		if err := a.addDomain(f, host, "", ""); err != nil {
			return err
		}
		// addDomain already saved and validated. Keep going for path/image
		// against the file on disk.
		var err error
		f, err = a.openConfigFile()
		if err != nil {
			return err
		}
	}
	if path != "" {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("healthcheck path must start with / (got %q)", path)
		}
		if err := f.SetString([]string{"deploy", "healthcheck", "path"}, path); err != nil {
			return err
		}
	}
	if image != "" {
		if err := f.SetString([]string{"image"}, strings.ToLower(image)); err != nil {
			return err
		}
	}
	if path != "" || image != "" {
		if err := f.Save(); err != nil {
			return err
		}
		if err := a.validateEditedConfig(f, ""); err != nil {
			return err
		}
	}

	a.log.Success("updated app %q", appName)
	if path != "" {
		a.log.Detail("healthcheck %s", path)
	}
	if image != "" {
		a.log.Detail("image %s", strings.ToLower(image))
	}
	return nil
}

func (a *App) addService(f *config.File, name, host, path string) error {
	// Hidden --service flag: same as `add app` plus optional --host.
	return a.addApp(f, name, host, path, "")
}

func (a *App) addDatabase(f *config.File, kind, name, disk string) error {
	kind = config.NormalizeAccessoryType(kind)
	if !config.SupportedAccessoryType(kind) {
		return fmt.Errorf("unknown database %q (want postgres or redis)", kind)
	}
	if name == "" {
		name = kind
	}
	if !config.ValidDNSLabel(name) {
		return fmt.Errorf("app name %q must be a lowercase DNS label", name)
	}
	if f.Lookup("accessories", name) != nil {
		return fmt.Errorf("%q already exists\n\n"+
			"hint: `buidl deploy %s` reconciles it; this command will not update an existing one", name, name)
	}

	node, err := accessoryNode(kind, disk)
	if err != nil {
		return err
	}
	if err := f.Set([]string{"accessories", name}, node); err != nil {
		return err
	}

	urlName := ""
	switch kind {
	case "postgres":
		urlName = "DATABASE_URL"
	case "redis":
		urlName = "REDIS_URL"
	}
	if urlName != "" {
		if err := f.AppendUnique([]string{"env", "secret"}, urlName); err != nil {
			return err
		}
	}
	if err := f.Save(); err != nil {
		return err
	}
	if err := a.validateEditedConfig(f, ""); err != nil {
		return err
	}

	if err := a.writeAccessorySecrets(f.App(), kind, name, urlName); err != nil {
		return err
	}

	a.log.Success("added %s app %q", kind, name)
	a.log.Detail("type: %s", kind)
	if disk != "" {
		a.log.Detail("storage: %s", disk)
	}
	a.log.Info("")
	a.log.Info("next: buidl deploy   # creates the app if it is missing")
	return nil
}

func accessoryNode(kind, disk string) (*yaml.Node, error) {
	raw := "type: " + kind + "\n"
	if disk != "" {
		raw += "storage: " + disk + "\n"
	}
	return config.ParseNode(raw)
}

func (a *App) writeAccessorySecrets(app, kind, accessoryName, urlName string) error {
	env := a.opts.environment
	if app == "" {
		app = "app"
	}
	host := config.AccessoryServiceName(app, accessoryName)

	switch kind {
	case "postgres":
		if secrets.Has(a.root, env, "POSTGRES_PASSWORD") {
			a.log.Detail("POSTGRES_PASSWORD already set; leaving it")
			return nil
		}
		pw, err := randomPassword()
		if err != nil {
			return err
		}
		rel, err := secrets.Set(a.root, env, "POSTGRES_PASSWORD", pw)
		if err != nil {
			return err
		}
		a.log.Detail("wrote POSTGRES_PASSWORD to %s", rel)
		if urlName != "" && !secrets.Has(a.root, env, urlName) {
			u := fmt.Sprintf("postgres://postgres:%s@%s:5432/%s", pw, host, app)
			if _, err := secrets.Set(a.root, env, urlName, u); err != nil {
				return err
			}
			a.log.Detail("wrote %s to %s", urlName, rel)
		}
	case "redis":
		if urlName != "" && !secrets.Has(a.root, env, urlName) {
			u := fmt.Sprintf("redis://%s:6379", host)
			rel, err := secrets.Set(a.root, env, urlName, u)
			if err != nil {
				return err
			}
			a.log.Detail("wrote %s to %s", urlName, rel)
		}
	}
	return nil
}

func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// isTemplateHost reports hosts `init` / `environment new` write before the
// user has given a real domain. Those are safe to replace.
func isTemplateHost(h string) bool {
	switch h {
	case "", "example.com", "staging.example.com", "${BUIDL_SLUG}.preview.example.com":
		return true
	default:
		return false
	}
}

func isSubdomainOrEqual(host, parent string) bool {
	if parent == "" {
		return false
	}
	return host == parent || strings.HasSuffix(host, "."+parent)
}

func hasRealProxyHost(f *config.File) bool {
	if h := f.String("proxy", "host"); h != "" && !isTemplateHost(h) {
		return true
	}
	for _, name := range f.EnvironmentNames() {
		if h := f.String("environments", name, "proxy", "host"); h != "" && !isTemplateHost(h) {
			return true
		}
	}
	return false
}

// syncEnvironmentHosts derives staging / production / preview hostnames from
// the app's public domain so the user never types those into the file.
func syncEnvironmentHosts(f *config.File, domain string) error {
	if containsString(f.EnvironmentNames(), "production") && isTemplateHost(f.String("environments", "production", "proxy", "host")) {
		if err := f.SetString([]string{"environments", "production", "proxy", "host"}, domain); err != nil {
			return err
		}
	}
	if containsString(f.EnvironmentNames(), "staging") && isTemplateHost(f.String("environments", "staging", "proxy", "host")) {
		if err := f.SetString([]string{"environments", "staging", "proxy", "host"}, "staging."+domain); err != nil {
			return err
		}
	}
	if containsString(f.EnvironmentNames(), "preview") && isTemplateHost(f.String("environments", "preview", "proxy", "host")) {
		if err := f.SetString([]string{"environments", "preview", "proxy", "host"}, "${BUIDL_SLUG}.preview."+domain); err != nil {
			return err
		}
	}
	if isTemplateHost(f.String("proxy", "host")) && f.Lookup("proxy") != nil {
		if err := f.SetString([]string{"proxy", "host"}, domain); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) proxyPrefix(f *config.File) []string {
	env := a.opts.environment
	if env == "" {
		env = f.DefaultEnvironment()
	}
	if env != "" && f.Lookup("environments", env) != nil {
		return []string{"environments", env, "proxy"}
	}
	return []string{"proxy"}
}

func (a *App) ensureInfra(f *config.File, user string, port int, email string) error {
	if f.String("infra", "ssh", "user") == "" {
		if user == "" {
			user = "root"
		}
		if err := f.SetString([]string{"infra", "ssh", "user"}, user); err != nil {
			return err
		}
	} else if user != "" && f.String("infra", "ssh", "user") != user {
		if err := f.SetString([]string{"infra", "ssh", "user"}, user); err != nil {
			return err
		}
	}
	if port != 0 {
		if err := f.Set([]string{"infra", "ssh", "port"}, &yaml.Node{
			Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(port),
		}); err != nil {
			return err
		}
	}
	if f.String("infra", "kubernetes", "distribution") == "" {
		if err := f.SetString([]string{"infra", "kubernetes", "distribution"}, "k3s"); err != nil {
			return err
		}
	}
	return a.setCertEmail(f, email)
}

func (a *App) setCertEmail(f *config.File, email string) error {
	if email == "" {
		return nil
	}
	if !strings.Contains(email, "@") {
		return fmt.Errorf("email must look like an address (got %q)", email)
	}
	existing := f.String("infra", "addons", "certManagerEmail")
	if existing != "" && existing != email {
		a.log.Detail("certManagerEmail already %s; leaving it", existing)
		return nil
	}
	return f.SetString([]string{"infra", "addons", "certManagerEmail"}, email)
}

func proxyWantsTLS(f *config.File) bool {
	if f.String("proxy", "ssl") == "true" || f.String("proxy", "host") != "" {
		return true
	}
	for _, env := range f.EnvironmentNames() {
		if f.String("environments", env, "proxy", "ssl") == "true" || f.String("environments", env, "proxy", "host") != "" {
			return true
		}
	}
	return false
}

func serverHosts(f *config.File) []string {
	n := f.Lookup("infra", "servers")
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	var hosts []string
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i < len(item.Content)-1; i += 2 {
			if item.Content[i].Value == "host" {
				hosts = append(hosts, item.Content[i+1].Value)
			}
		}
	}
	return hosts
}
