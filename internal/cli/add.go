package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/secrets"
)

// newAddCmd writes a database, cache, or this app's hostname into buidl.yaml.
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
		Use:   "add",
		Short: "Add a database, cache, or service to the stack",
		Long: `Write a database, cache, or this app's hostname into buidl.yaml.

  buidl add --database postgres
  buidl add --database redis
  buidl add --service --host api.example.com

A typed accessory is just ` + "`type: postgres`" + ` in the file. Image, port,
volume and POSTGRES_PASSWORD are filled at load. A first ` + "`buidl deploy`" + `
creates it if it is missing; later deploys leave it alone.

Multiple application services in one file are not supported yet. Naming a
second service is an error rather than a half-written schema.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if database != "" && service {
				return fmt.Errorf("pass --database or --service, not both")
			}
			if database == "" && !service {
				return fmt.Errorf("pass --database postgres or --service\n\nhint: `buidl add --database postgres` adds a Postgres accessory")
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
	return cmd
}

func (a *App) addService(f *config.File, name, host, path string) error {
	appName := f.App()
	if appName == "" {
		return fmt.Errorf("%s has no `app`", a.path)
	}
	if name != "" && name != appName {
		return fmt.Errorf("this file already defines app %q; multiple services in one stack are not supported yet\n\n"+
			"hint: run `buidl init` in another directory for a second app, or configure this one:\n"+
			"  buidl add --service --host %s.example.com", appName, name)
	}
	if host == "" && path == "" {
		return fmt.Errorf("app %q is already the service in this file\n\n"+
			"hint: pass --host or --path to configure it", appName)
	}

	env := a.opts.environment
	if env == "" {
		env = f.DefaultEnvironment()
	}

	if host != "" {
		if strings.ContainsAny(host, "/:") {
			return fmt.Errorf("host must be a bare hostname without scheme or port (got %q)", host)
		}
		hostPath := []string{"proxy", "host"}
		if env != "" && f.Lookup("environments", env) != nil {
			hostPath = []string{"environments", env, "proxy", "host"}
		}
		if err := f.SetString(hostPath, host); err != nil {
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
	if err := f.Save(); err != nil {
		return err
	}
	if err := a.validateEditedConfig(f, ""); err != nil {
		return err
	}

	a.log.Success("updated app %q", appName)
	if host != "" {
		a.log.Detail("host %s", host)
	}
	if path != "" {
		a.log.Detail("healthcheck %s", path)
	}
	return nil
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
