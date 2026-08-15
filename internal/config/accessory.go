package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Typed accessory images and sizes applied when `type` is set and the field
// is omitted. They are defaults, not pins: a user who needs a specific tag
// writes `image` next to `type`.
const (
	DefaultPostgresImage   = "postgres:17"
	DefaultPostgresPort    = int32(5432)
	DefaultPostgresStorage = "10Gi"
	DefaultRedisImage      = "redis:7"
	DefaultRedisPort       = int32(6379)
	DefaultRedisStorage    = "1Gi"
)

// NormalizeAccessoryType maps aliases onto the types `add postgres` writes.
func NormalizeAccessoryType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "postgres", "postgresql", "pg":
		return "postgres"
	case "redis", "valkey":
		return "redis"
	default:
		return strings.ToLower(strings.TrimSpace(t))
	}
}

// SupportedAccessoryType reports whether t is a typed accessory buidl knows.
func SupportedAccessoryType(t string) bool {
	switch NormalizeAccessoryType(t) {
	case "postgres", "redis":
		return true
	default:
		return false
	}
}

// applyAccessoryDefaults fills a typed accessory so `type: postgres` is a
// complete StatefulSet spec, then applies the image-based mountPath guess.
func applyAccessoryDefaults(app, name string, acc Accessory) Accessory {
	switch NormalizeAccessoryType(acc.Type) {
	case "postgres":
		if acc.Image == "" {
			acc.Image = DefaultPostgresImage
		}
		if acc.Port == 0 {
			acc.Port = DefaultPostgresPort
		}
		if acc.Storage == "" {
			acc.Storage = DefaultPostgresStorage
		}
		acc.Env.Secret = appendUnique(acc.Env.Secret, "POSTGRES_PASSWORD")
		if acc.Env.Clear == nil {
			acc.Env.Clear = map[string]string{}
		}
		if acc.Env.Clear["POSTGRES_USER"] == "" {
			acc.Env.Clear["POSTGRES_USER"] = "postgres"
		}
		if acc.Env.Clear["POSTGRES_DB"] == "" {
			acc.Env.Clear["POSTGRES_DB"] = app
		}
	case "redis":
		if acc.Image == "" {
			acc.Image = DefaultRedisImage
		}
		if acc.Port == 0 {
			acc.Port = DefaultRedisPort
		}
		if acc.Storage == "" {
			acc.Storage = DefaultRedisStorage
		}
	}

	if acc.MountPath == "" && acc.Storage != "" {
		acc.MountPath = defaultMountPath(acc.Image)
	}
	return acc
}

// AccessoryServiceName is the in-cluster DNS name of an accessory, matching
// release.ObjectName(app, name) for names that fit the Kubernetes limit.
func AccessoryServiceName(app, name string) string {
	return app + "-" + name
}

// SecretNames returns every secret a deploy must resolve: the app's
// env.secret plus each accessory's env.secret.
//
// Accessory-only names (POSTGRES_PASSWORD on type: postgres) live in the
// accessory's Secret. They are not injected into the app unless the user
// also listed them under env.secret.
func (c *Config) SecretNames() []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]bool, len(c.Env.Secret)+len(c.Accessories))
	var out []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, name := range c.Env.Secret {
		add(name)
	}
	accNames := make([]string, 0, len(c.Accessories))
	for name := range c.Accessories {
		accNames = append(accNames, name)
	}
	sort.Strings(accNames)
	for _, name := range accNames {
		for _, secret := range c.Accessories[name].Env.Secret {
			add(secret)
		}
	}
	return out
}

// SynthesizeAccessoryURLs derives connection URLs from typed accessories
// when the app declared the name and no value is already present.
//
// Existing values win. An app pointed at RDS must not have that URL overwritten
// because a Postgres accessory happens to be in the file.
func SynthesizeAccessoryURLs(c *Config, values map[string]string) map[string]string {
	if c == nil || values == nil {
		return nil
	}
	declared := map[string]bool{}
	for _, name := range c.Env.Secret {
		declared[name] = true
	}

	out := map[string]string{}
	for name, acc := range c.Accessories {
		switch NormalizeAccessoryType(acc.Type) {
		case "postgres":
			if !declared["DATABASE_URL"] || values["DATABASE_URL"] != "" {
				continue
			}
			if url := postgresURL(c.App, name, acc, values["POSTGRES_PASSWORD"]); url != "" {
				out["DATABASE_URL"] = url
			}
		case "redis":
			if !declared["REDIS_URL"] || values["REDIS_URL"] != "" {
				continue
			}
			if url := redisURL(c.App, name, acc); url != "" {
				out["REDIS_URL"] = url
			}
		}
	}
	return out
}

func postgresURL(app, name string, acc Accessory, password string) string {
	if password == "" {
		return ""
	}
	user := "postgres"
	db := app
	if acc.Env.Clear != nil {
		if acc.Env.Clear["POSTGRES_USER"] != "" {
			user = acc.Env.Clear["POSTGRES_USER"]
		}
		if acc.Env.Clear["POSTGRES_DB"] != "" {
			db = acc.Env.Clear["POSTGRES_DB"]
		}
	}
	port := acc.Port
	if port == 0 {
		port = DefaultPostgresPort
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   fmt.Sprintf("%s:%d", AccessoryServiceName(app, name), port),
		Path:   "/" + db,
	}
	return u.String()
}

func redisURL(app, name string, acc Accessory) string {
	port := acc.Port
	if port == 0 {
		port = DefaultRedisPort
	}
	u := url.URL{
		Scheme: "redis",
		Host:   fmt.Sprintf("%s:%d", AccessoryServiceName(app, name), port),
	}
	return u.String()
}

func appendUnique(list []string, value string) []string {
	for _, v := range list {
		if v == value {
			return list
		}
	}
	return append(list, value)
}
