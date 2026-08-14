// Package deploy defines the backend-agnostic contract for shipping a release.
//
// Kubernetes is the only implemented backend today, but every operation is
// expressed in terms that a bare-metal SSH backend can also satisfy — plan,
// apply, wait for health, roll back. Keeping that boundary honest now is what
// makes "cloud and bare metal and everything in between" achievable later
// without reworking the command layer.
package deploy

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/release"
)

// Request carries everything a backend needs for one operation.
type Request struct {
	Config  *config.Config
	Release release.Release
	// Root is the directory containing buidl.yaml.
	Root string
	// Secrets holds resolved values for SecretNames(): the app's env.secret
	// and each accessory's env.secret. Populated at deploy time from the
	// local environment or a secrets provider; never persisted by buidl.
	// Accessory-only names stay off the app container.
	Secrets map[string]string
	// Wait blocks until the release is healthy. Almost always true: a deploy
	// command that returns before the app is up is useless as a CI gate.
	Wait bool
	// AutoRollback reverts to the previous release if the rollout fails to become
	// healthy within the deploy timeout.
	AutoRollback bool
	// Prune deletes managed objects that are no longer in the desired state.
	Prune bool
}

// Action classifies what a plan step will do to one object.
type Action string

const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionUnchanged Action = "unchanged"
	ActionDelete    Action = "delete"
)

// FieldChange is one meaningful field-level difference within an object.
//
// A raw YAML diff answers "did anything change"; this answers "what changed", in
// the terms the user actually wrote. Seeing `image sha256:abc → sha256:def` and
// `replicas 2 → 5` is what makes a plan reviewable, whereas forty lines of
// server-defaulted YAML is not.
type FieldChange struct {
	// Field is a readable name, e.g. "image" or "replicas".
	Field string
	From  string
	To    string
}

// String renders the change as "field: from → to".
func (f FieldChange) String() string {
	from, to := f.From, f.To
	if from == "" {
		from = "(unset)"
	}
	if to == "" {
		to = "(unset)"
	}
	return fmt.Sprintf("%s: %s → %s", f.Field, from, to)
}

// Change is one object-level entry in a plan.
type Change struct {
	Action Action
	Kind   string
	Name   string
	// Diff is a unified diff of the object, present for ActionUpdate.
	Diff string
	// Summary is a one-line human description of the change.
	Summary string

	// Fields lists the meaningful field-level differences, when they could be
	// determined.
	Fields []FieldChange

	// Impact describes the runtime consequence, e.g. "restarts 3 instances".
	// Empty when the change is inert, such as a label edit.
	Impact string

	// Applied reports whether this change was actually applied. Used when a
	// deploy fails partway: the changes already applied are exactly what a user
	// needs to know to understand the cluster's current state.
	Applied bool
	// Err is the failure for this object, when applying it failed.
	Err error
}

// FieldSummary renders the field changes on one line, for compact output.
func (c Change) FieldSummary() string {
	if len(c.Fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(c.Fields))
	for _, f := range c.Fields {
		parts = append(parts, f.String())
	}
	return strings.Join(parts, ", ")
}

// Plan is the result of a dry run: exactly what a deploy would change.
//
// This is the safety property that distinguishes a deploy tool from a shell
// script. `buidl plan` in a pull request shows a reviewer the infrastructure
// delta before it is applied.
type Plan struct {
	Environment string
	Release     release.Release
	Changes     []Change
	// Warnings are non-fatal concerns worth a reviewer's attention, such as
	// deploying a dirty tree or removing the last replica.
	Warnings []string
}

// HasChanges reports whether the plan would modify anything.
func (p *Plan) HasChanges() bool {
	for _, c := range p.Changes {
		if c.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

// Counts tallies changes by action, for a summary line.
func (p *Plan) Counts() map[Action]int {
	counts := map[Action]int{}
	for _, c := range p.Changes {
		counts[c.Action]++
	}
	return counts
}

// Outcome reports the result of a deploy or rollback.
type Outcome struct {
	Release release.Release
	// PreviousRelease is the release that was live before, and the target of an
	// automatic or manual rollback.
	PreviousRelease string
	// URL is the primary external address, when the backend knows one.
	URL string
	// Duration is the wall-clock time from apply to healthy.
	Duration time.Duration
	// RolledBack reports that the deploy failed and was reverted.
	RolledBack bool
	Changes    []Change

	// Partial reports that the deploy failed partway through applying objects, so
	// some changes landed and others did not. The cluster is in a mixed state and
	// the user needs to be told which is which.
	Partial bool

	// Instances describes the running instances after the deploy, so a successful
	// run ends by reporting what is actually serving rather than only that the
	// apply succeeded.
	Instances []PodStatus
}

// Applied returns the changes that reached the cluster.
func (o *Outcome) Applied() []Change {
	var out []Change
	for _, c := range o.Changes {
		if c.Applied {
			out = append(out, c)
		}
	}
	return out
}

// Failed returns the changes that errored.
func (o *Outcome) Failed() []Change {
	var out []Change
	for _, c := range o.Changes {
		if c.Err != nil {
			out = append(out, c)
		}
	}
	return out
}

// Status describes what is currently live.
type Status struct {
	Environment string
	// Release currently serving traffic.
	Release string
	Digest  string
	Image   string
	// Ready and Desired are replica counts.
	Ready   int32
	Desired int32
	Updated int32
	// Available reports whether the workload meets its availability requirement.
	Available bool
	// Conditions are backend-reported condition messages.
	Conditions []string
	URL        string
	DeployedAt time.Time
	DeployedBy string
	GitSHA     string
	// Pods describes individual instances, for diagnosing a partial rollout.
	Pods []PodStatus
}

// PodStatus is one running instance.
type PodStatus struct {
	Name     string
	Phase    string
	Ready    bool
	Restarts int32
	Age      time.Duration
	Node     string
	// Message explains a non-running pod, e.g. an image pull failure.
	Message string
	Release string
}

// ReleaseInfo is one entry in the deploy history.
type ReleaseInfo struct {
	ID     string
	Digest string
	// Live marks the release currently serving traffic.
	Live       bool
	Revision   int64
	CreatedAt  time.Time
	DeployedBy string
	GitSHA     string
	GitBranch  string
	Replicas   int32
}

// RollbackRequest asks a backend to revert.
type RollbackRequest struct {
	Config *config.Config
	Root   string
	// To names a specific release. Empty means "the previous release".
	To   string
	Wait bool
}

// LogRequest streams application logs.
type LogRequest struct {
	Config *config.Config
	// Follow streams indefinitely.
	Follow bool
	// Tail is the number of recent lines to start from; -1 means all.
	Tail int64
	// Since limits output to logs newer than this duration.
	Since time.Duration
	// Release filters to one release's instances. Empty means the live release.
	Release string
	// Out receives the log stream.
	Out io.Writer
}

// Target is a deployment backend.
type Target interface {
	// Name identifies the backend in output.
	Name() string

	// Preflight validates that a deploy can succeed before anything is changed:
	// credentials work, the cluster is reachable, the image exists, required
	// secrets are present. Every failure it can catch here is a failure that
	// would otherwise happen halfway through a rollout.
	Preflight(ctx context.Context, req Request) error

	// Plan performs a dry run and reports the changes a deploy would make.
	Plan(ctx context.Context, req Request) (*Plan, error)

	// Deploy applies the release and, when Wait is set, blocks until healthy.
	Deploy(ctx context.Context, req Request) (*Outcome, error)

	// Rollback reverts to a previous release.
	Rollback(ctx context.Context, req RollbackRequest) (*Outcome, error)

	// Status reports what is currently live.
	Status(ctx context.Context, req Request) (*Status, error)

	// Releases lists deploy history, newest first.
	Releases(ctx context.Context, req Request) ([]ReleaseInfo, error)

	// Logs streams application logs.
	Logs(ctx context.Context, req LogRequest) error

	// Destroy tears the environment down. For an ephemeral preview that means
	// the namespace; for a long-lived environment, only the app objects.
	Destroy(ctx context.Context, req DestroyRequest) (*DestroyOutcome, error)

	// Close releases connections.
	Close() error
}

// Registry maps a target name to its constructor, so backends can be added
// without the command layer knowing about them.
type Registry map[string]Factory

// Factory constructs a Target for a config.
type Factory func(cfg *config.Config, log Logger) (Target, error)

// Logger is the output surface backends use.
type Logger interface {
	Info(format string, args ...any)
	Detail(format string, args ...any)
	Warn(format string, args ...any)
	Success(format string, args ...any)
	Step(name string)
	// StepDetail annotates the current step with a short outcome note, shown in
	// the closing summary.
	StepDetail(format string, args ...any)
}

var registry = Registry{}

// Register adds a backend. Called from backend packages' init functions.
func Register(name string, f Factory) {
	registry[name] = f
}

// For constructs the backend named by cfg.Deploy.Target.
func For(cfg *config.Config, log Logger) (Target, error) {
	f, ok := registry[cfg.Deploy.Target]
	if !ok {
		return nil, fmt.Errorf("unknown deploy target %q (available: %s)", cfg.Deploy.Target, available())
	}
	return f(cfg, log)
}

func available() string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "none registered"
	}
	out := names[0]
	for _, n := range names[1:] {
		out += ", " + n
	}
	return out
}
