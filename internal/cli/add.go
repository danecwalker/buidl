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

// newAddCmd writes a server, domain, database, or this app's settings into buidl.yaml.
func newAddCmd(a *App) *cobra.Command {
	var (
		database string
		service  bool
		name     string
		host     string
		path     string
		disk     string
	)

	cmd := &cobra.Command{
		Use:          "add",
		SilenceUsage: true,
		Short:        "Add a server, domain, database, or app to the stack",
		Long: `Write a server, domain, database, or this app's settings into buidl.yaml.

  buidl add server 203.0.113.10 --email you@example.com
  buidl add domain example.com
  buidl add domain api.example.com
  buidl add postgres
  buidl add redis

A second domain is an extra hostname on this app (www, api.example.com, …).
They share one Ingress and one certificate. A separate API service is a
second app, which is not in this file yet.

A typed accessory is just ` + "`type: postgres`" + ` in the file. Image, port,
volume and POSTGRES_PASSWORD are filled at load. A first ` + "`buidl deploy`" + `
creates it if it is missing; later deploys leave it alone.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if database != "" && service {
				return fmt.Errorf("pass --database or --service, not both")
			}
			if database == "" && !service {
				return cmd.Help()
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
			return a.addDatabase(f, database, name, disk)
		},
	}

	f := cmd.Flags()
	f.StringVar(&database, "database", "", "add a typed accessory: postgres or redis")
	f.BoolVar(&service, "service", false, "configure this app's host or health path")
	f.StringVar(&name, "name", "", "accessory name (default: the database type)")
	f.StringVar(&host, "host", "", "proxy hostname for this app")
	f.StringVar(&path, "path", "", "healthcheck path")
	f.StringVar(&disk, "disk", "", "persistent volume size (e.g. 20Gi)")
	_ = f.MarkHidden("database")
	_ = f.MarkHidden("service")
	_ = f.MarkHidden("name")
	_ = f.MarkHidden("host")
	_ = f.MarkHidden("path")
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
	var email string

	cmd := &cobra.Command{
		Use:   "domain HOST",
		Short: "Add a hostname this app should serve",
		Long: `Write a hostname into the proxy. The first call is the primary
host. Later calls (www, api.example.com, …) are aliases on the same app:
one Ingress, one certificate, every name on the same Service.

  buidl add domain example.com --email you@example.com
  buidl add domain api.example.com
  buidl add domain www.example.com

A separate API process is a second app, not a second domain.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			return a.addDomain(f, args[0], email)
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "Let's Encrypt contact (infra.addons.certManagerEmail)")
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
		Short: "Add a " + kind + " accessory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := a.openConfigFile()
			if err != nil {
				return err
			}
			return a.addDatabase(f, kind, name, disk)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "accessory name (default: "+kind+")")
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
		Use:   "app [NAME]",
		Short: "Configure this app's host, health path, or image",
		Long: `Configure the app already in this file.

  buidl add app --host example.com
  buidl add app --path /up
  buidl add app --image ghcr.io/acme/web

A second named app in one stack is not supported yet.`,
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
			return a.addApp(f, name, host, path, image)
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

func (a *App) addDomain(f *config.File, host, email string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("domain is required")
	}
	if strings.ContainsAny(host, "/:") {
		return fmt.Errorf("host must be a bare hostname without scheme or port (got %q)", host)
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

	if primary == "" {
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

	if primary == "" {
		a.log.Success("added domain %s", host)
	} else {
		a.log.Success("added alias %s (with %s)", host, primary)
	}
	a.log.Detail("tls on")
	return nil
}

func (a *App) addApp(f *config.File, name, host, path, image string) error {
	appName := f.App()
	if appName == "" {
		return fmt.Errorf("%s has no `app`", a.path)
	}
	if name != "" && name != appName {
		return fmt.Errorf("this file already defines app %q; a second app in one stack is not supported yet\n\n"+
			"hint: run `buidl init` in another directory for now, or configure this one:\n"+
			"  buidl add domain %s.example.com", appName, name)
	}
	if host == "" && path == "" && image == "" {
		return fmt.Errorf("app %q is already the service in this file\n\n"+
			"hint: pass --host, --path, or --image to configure it", appName)
	}

	if host != "" {
		if err := a.addDomain(f, host, ""); err != nil {
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
		return fmt.Errorf("accessory name %q must be a lowercase DNS label", name)
	}
	if f.Lookup("accessories", name) != nil {
		return fmt.Errorf("accessory %q already exists\n\n"+
			"hint: `buidl accessory apply` reconciles it; this command will not update an existing one", name)
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

	a.log.Success("added %s accessory %q", kind, name)
	a.log.Detail("type: %s", kind)
	if disk != "" {
		a.log.Detail("storage: %s", disk)
	}
	a.log.Info("")
	a.log.Info("next: buidl deploy   # creates the accessory if it is missing")
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
