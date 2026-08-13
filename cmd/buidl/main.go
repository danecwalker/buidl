// Command buidl builds and deploys applications to Kubernetes, cloud, and bare
// metal.
package main

import (
	"os"

	"github.com/danecwalker/buidl/internal/cli"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/buidl
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Execute())
}
