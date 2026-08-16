# Contributing

## Getting set up

```sh
make test        # unit tests — no cluster, no network
make lint
make build
```

## What the tests are for

**Unit tests** cover pure logic and must never require a cluster, a network, or
a running daemon. If a test needs any of those, it belongs in the acceptance
suite instead.

**Acceptance tests** (`test/acceptance/run.sh`) run against a real cluster and a
real registry. They exist because the unit tests structurally cannot catch the
bugs that matter most here: a use-after-close on a cached SSH connection, an
image `USER` that Kubernetes refuses to verify, a rollback losing a write race
to the Deployment controller. Every one of those passed the unit suite cleanly.

```sh
export DEMO_SECRET=any-value-for-testing
make acceptance
```

`examples/hello` is an app built to misbehave on demand — `FAIL_READINESS=1`,
`CRASH_ON_BOOT=1`, `BOOT_DELAY=25s` — because the happy path is the easy part
and the failure reporting is what needs proving.

## Product rules

**Users do not edit `buidl.yaml` unless they are doing something advanced.**

The happy path is commands: `init`, `add server`, `add domain`, `add postgres`,
`add api`, `deploy`. Those commands write the file. A first-run deploy that dies telling
the user to add a key is a bug in the default or in the command that should
have written it.

`init` is a setup wizard. If something must be chosen (CI, staging, review
apps), ask on a terminal or take a flag. Do not leave the user to paste a
workflow or an `environments` block.

- Prefer a safe omitted default (`createPullSecret`, `createNamespace`,
  `strategy: bluegreen`) over a preflight hint that says "edit the YAML".
- If a setting must be chosen, add a flag to `init` / `add` / `variable` that
  writes it.
- `init` may write the resolved default so the generated file shows what will
  happen. That is the CLI writing the file, not the user.
- Errors may mention a key as an advanced override. They must not make
  opening `buidl.yaml` the only way through the happy path.

## House style

**Comments explain why, not what.** A comment that restates its code is worse
than no comment, because it goes stale silently. The bar is: would a competent
reader wonder why this is written this way? If so, answer that — and say what
breaks if it were done the obvious way.

Good:

```go
// A Deployment's selector is immutable, so including the release ID would make
// the second deploy fail permanently.
```

Bad:

```go
// Set the selector labels.
```

**Errors tell the user what to do.** An error that names a problem without a
next step is half-finished. Where there is a known fix, print it:

```go
return fmt.Errorf("namespace %q does not exist\n\n"+
    "hint: create it with kubectl, or drop createNamespace: false so buidl creates it",
    ns)
```

**Tests say what they guard against.** Each non-obvious assertion carries a
comment naming the real failure it prevents, so a future reader can tell whether
changing the behaviour is a fix or a regression.

**Secret values never reach output.** Report presence, length, or the key name.
Plan output ends up in pull requests and CI logs; there are tests that fail if a
value leaks into any output path, and they should stay that way.

## Before opening a pull request

```sh
gofmt -l .          # must be empty
go vet ./...
go test -race ./...
```

CI runs all of the above plus cross-compilation and a smoke test that
`buidl init` produces a config passing buidl's own validation.

If you change anything a user sees — a command, a config key, an error message —
update the README in the same change. Documentation that drifts from behaviour
is a bug report waiting to happen.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not open a public issue for a vulnerability.
