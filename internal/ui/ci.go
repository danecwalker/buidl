package ui

import (
	"fmt"
	"os"
	"strings"
)

// CI describes the detected continuous integration environment.
//
// buidl uses this for three things: choosing plain output, folding logs into
// collapsible groups, and emitting native annotations so warnings and errors
// appear in the provider's run summary instead of only in raw log text.
type CI struct {
	Detected bool
	// Provider is a stable identifier, e.g. "github-actions".
	Provider string
	// Name is human-readable, e.g. "GitHub Actions".
	Name string
	// Actor is the user or service that triggered the run, recorded on releases.
	Actor string
	// PullRequest is the PR/MR number when the run is for one, else "".
	PullRequest string
	// RunURL links back to the CI run, recorded on releases for traceability.
	RunURL string
}

// DetectCI inspects the environment for a known CI provider.
//
// Ordered most-specific first: several providers also set the generic CI=true,
// so the generic check must come last.
func DetectCI() CI {
	env := os.Getenv

	switch {
	case env("GITHUB_ACTIONS") == "true":
		ci := CI{Detected: true, Provider: "github-actions", Name: "GitHub Actions", Actor: env("GITHUB_ACTOR")}
		if server, repo, id := env("GITHUB_SERVER_URL"), env("GITHUB_REPOSITORY"), env("GITHUB_RUN_ID"); repo != "" && id != "" {
			if server == "" {
				server = "https://github.com"
			}
			ci.RunURL = fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, id)
		}
		// GITHUB_REF is refs/pull/<n>/merge on pull_request events.
		if ref := env("GITHUB_REF"); strings.HasPrefix(ref, "refs/pull/") {
			parts := strings.Split(ref, "/")
			if len(parts) > 2 {
				ci.PullRequest = parts[2]
			}
		}
		return ci

	case env("GITLAB_CI") == "true":
		return CI{
			Detected:    true,
			Provider:    "gitlab-ci",
			Name:        "GitLab CI",
			Actor:       env("GITLAB_USER_LOGIN"),
			PullRequest: env("CI_MERGE_REQUEST_IID"),
			RunURL:      env("CI_PIPELINE_URL"),
		}

	case env("BUILDKITE") == "true":
		return CI{
			Detected:    true,
			Provider:    "buildkite",
			Name:        "Buildkite",
			Actor:       env("BUILDKITE_BUILD_CREATOR"),
			PullRequest: prOrEmpty(env("BUILDKITE_PULL_REQUEST")),
			RunURL:      env("BUILDKITE_BUILD_URL"),
		}

	case env("CIRCLECI") == "true":
		return CI{
			Detected: true,
			Provider: "circleci",
			Name:     "CircleCI",
			Actor:    env("CIRCLE_USERNAME"),
			RunURL:   env("CIRCLE_BUILD_URL"),
		}

	case env("TF_BUILD") == "True":
		return CI{
			Detected: true,
			Provider: "azure-pipelines",
			Name:     "Azure Pipelines",
			Actor:    env("BUILD_REQUESTEDFOR"),
		}

	case env("JENKINS_URL") != "":
		return CI{
			Detected: true,
			Provider: "jenkins",
			Name:     "Jenkins",
			RunURL:   env("BUILD_URL"),
		}

	case env("CI") != "" && env("CI") != "false" && env("CI") != "0":
		return CI{Detected: true, Provider: "generic", Name: "CI"}
	}

	return CI{}
}

// prOrEmpty normalizes Buildkite's "false" sentinel for non-PR builds.
func prOrEmpty(v string) string {
	if v == "" || v == "false" {
		return ""
	}
	return v
}

// GroupStart returns the provider directive that opens a collapsible log group,
// or "" if the provider has none.
func (c CI) GroupStart(name string) string {
	switch c.Provider {
	case "github-actions":
		return "::group::" + name
	case "azure-pipelines":
		return "##[group]" + name
	case "buildkite":
		// A leading "---" makes the group collapsed by default.
		return "--- " + name
	case "travis":
		return "travis_fold:start:" + foldName(name)
	}
	return ""
}

// GroupEnd returns the directive that closes the current group.
func (c CI) GroupEnd() string {
	switch c.Provider {
	case "github-actions":
		return "::endgroup::"
	case "azure-pipelines":
		return "##[endgroup]"
	}
	// Buildkite groups are implicitly closed by the next "---".
	return ""
}

// Annotate renders a native warning/error annotation, or "" when the provider
// has no annotation syntax (in which case the caller falls back to plain text).
func (c CI) Annotate(level Level, msg string) string {
	// Annotations are single-line; embedded newlines would break the directive.
	msg = escapeAnnotation(msg)

	switch c.Provider {
	case "github-actions":
		switch level {
		case LevelWarn:
			return "::warning::" + msg
		case LevelError:
			return "::error::" + msg
		}
	case "azure-pipelines":
		switch level {
		case LevelWarn:
			return "##vso[task.logissue type=warning]" + msg
		case LevelError:
			return "##vso[task.logissue type=error]" + msg
		}
	}
	return ""
}

// escapeAnnotation flattens and escapes text for a single-line CI directive.
func escapeAnnotation(msg string) string {
	msg = strings.ReplaceAll(msg, "\r\n", " ")
	msg = strings.ReplaceAll(msg, "\n", " ")
	// GitHub treats %, \r and \n specially inside workflow commands.
	msg = strings.ReplaceAll(msg, "%", "%25")
	return msg
}

func foldName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' {
			return '_'
		}
		return r
	}, strings.ToLower(name))
}

// SetOutput emits a provider directive that exposes a key/value to later CI
// steps, so a workflow can consume the release ID or URL buidl produced.
//
// Returns "" when the provider has no such mechanism.
func (c CI) SetOutput(key, value string) string {
	switch c.Provider {
	case "github-actions":
		// Writing to $GITHUB_OUTPUT is the supported mechanism; the deprecated
		// ::set-output:: directive is disabled on current runners. The caller
		// appends this line to the file named by GITHUB_OUTPUT.
		return fmt.Sprintf("%s=%s", key, value)
	case "buildkite":
		return fmt.Sprintf("buildkite-agent meta-data set %q %q", key, value)
	}
	return ""
}

// OutputFile returns the path a provider expects step outputs to be appended to.
func (c CI) OutputFile() string {
	if c.Provider == "github-actions" {
		return os.Getenv("GITHUB_OUTPUT")
	}
	return ""
}
