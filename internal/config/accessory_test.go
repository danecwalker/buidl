package config

import (
	"strings"
	"testing"
)

func TestTypedPostgresDefaults(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
accessories:
  postgres:
    type: postgres
`), Strict: true})
	if err != nil {
		t.Fatalf("type: postgres must load without an image: %v", err)
	}
	acc := res.Config.Accessories["postgres"]
	if acc.Image != DefaultPostgresImage {
		t.Errorf("image = %q, want %s", acc.Image, DefaultPostgresImage)
	}
	if acc.Port != DefaultPostgresPort {
		t.Errorf("port = %d", acc.Port)
	}
	if acc.Storage != DefaultPostgresStorage {
		t.Errorf("storage = %q", acc.Storage)
	}
	if acc.MountPath != "/var/lib/postgresql/data" {
		t.Errorf("mountPath = %q", acc.MountPath)
	}
	if acc.Env.Clear["POSTGRES_USER"] != "postgres" {
		t.Errorf("POSTGRES_USER = %q", acc.Env.Clear["POSTGRES_USER"])
	}
	if acc.Env.Clear["POSTGRES_DB"] != "web" {
		t.Errorf("POSTGRES_DB = %q, want the app name so DATABASE_URL matches", acc.Env.Clear["POSTGRES_DB"])
	}
	if !containsString(acc.Env.Secret, "POSTGRES_PASSWORD") {
		t.Errorf("secret = %v, want POSTGRES_PASSWORD", acc.Env.Secret)
	}
}

func TestTypedRedisDefaults(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
accessories:
  redis:
    type: redis
`), Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	acc := res.Config.Accessories["redis"]
	if acc.Image != DefaultRedisImage {
		t.Errorf("image = %q", acc.Image)
	}
	if acc.Port != DefaultRedisPort {
		t.Errorf("port = %d", acc.Port)
	}
	if acc.MountPath != "/data" {
		t.Errorf("mountPath = %q", acc.MountPath)
	}
}

func TestTypedAccessoryKeepsExplicitImage(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
accessories:
  postgres:
    type: postgres
    image: postgres:16
    storage: 20Gi
`), Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	acc := res.Config.Accessories["postgres"]
	if acc.Image != "postgres:16" {
		t.Errorf("explicit image was overwritten: %q", acc.Image)
	}
	if acc.Storage != "20Gi" {
		t.Errorf("explicit storage was overwritten: %q", acc.Storage)
	}
}

func TestUnknownAccessoryTypeRejected(t *testing.T) {
	_, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
accessories:
  db:
    type: neon
`), Strict: true})
	if err == nil {
		t.Fatal("expected unknown type to fail")
	}
	if !strings.Contains(err.Error(), "neon") {
		t.Errorf("error should name the type, got: %v", err)
	}
}

func TestSynthesizeAccessoryURLs(t *testing.T) {
	cfg := &Config{
		App: "web",
		Env: Env{Secret: []string{"DATABASE_URL", "REDIS_URL"}},
		Accessories: map[string]Accessory{
			"postgres": {
				Type: "postgres",
				Port: 5432,
				Env: Env{Clear: map[string]string{
					"POSTGRES_USER": "postgres",
					"POSTGRES_DB":   "web",
				}},
			},
			"redis": {Type: "redis", Port: 6379},
		},
	}

	got := SynthesizeAccessoryURLs(cfg, map[string]string{"POSTGRES_PASSWORD": "s3cret"})
	if !strings.Contains(got["DATABASE_URL"], "postgres://postgres:s3cret@web-postgres:5432/web") {
		t.Errorf("DATABASE_URL = %q", got["DATABASE_URL"])
	}
	if got["REDIS_URL"] != "redis://web-redis:6379" {
		t.Errorf("REDIS_URL = %q", got["REDIS_URL"])
	}

	// An existing URL is the user's choice (RDS, a managed cache) and must win.
	existing := map[string]string{
		"POSTGRES_PASSWORD": "s3cret",
		"DATABASE_URL":      "postgres://rds.example.com/web",
	}
	got = SynthesizeAccessoryURLs(cfg, existing)
	if got["DATABASE_URL"] != "" {
		t.Errorf("existing DATABASE_URL was overwritten: %q", got["DATABASE_URL"])
	}
}

func TestSecretNamesIncludesAccessorySecrets(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
env:
  secret: [DATABASE_URL]
accessories:
  postgres:
    type: postgres
  redis:
    type: redis
`), Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Config.SecretNames()
	if !containsString(got, "DATABASE_URL") {
		t.Errorf("SecretNames = %v, want DATABASE_URL", got)
	}
	if !containsString(got, "POSTGRES_PASSWORD") {
		t.Errorf("SecretNames = %v, want the accessory password", got)
	}
	if got[0] != "DATABASE_URL" {
		t.Errorf("app env.secret should come first, got %v", got)
	}
	// Redis has no required secret; do not invent one.
	if containsString(got, "REDIS_URL") {
		t.Errorf("undeclared REDIS_URL should not be required: %v", got)
	}
}

func TestSecretNamesDedupesAppAndAccessory(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
env:
  secret: [POSTGRES_PASSWORD, DATABASE_URL]
accessories:
  postgres:
    type: postgres
`), Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Config.SecretNames()
	n := 0
	for _, name := range got {
		if name == "POSTGRES_PASSWORD" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("POSTGRES_PASSWORD appeared %d times in %v", n, got)
	}
}

func TestSynthesizeSkipsUndeclared(t *testing.T) {
	cfg := &Config{
		App: "web",
		Accessories: map[string]Accessory{
			"postgres": {Type: "postgres", Port: 5432},
		},
	}
	got := SynthesizeAccessoryURLs(cfg, map[string]string{"POSTGRES_PASSWORD": "x"})
	if len(got) != 0 {
		t.Errorf("undeclared DATABASE_URL should not be injected: %v", got)
	}
}
