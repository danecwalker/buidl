BINARY := buidl
PKG    := github.com/danewalker/buidl/cmd/buidl

# Prefer a tag; fall back to a short sha so a dev build is still identifiable.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: build
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

.PHONY: install
install:
	go install -trimpath -ldflags "$(LDFLAGS)" $(PKG)

.PHONY: test
test:
	go test ./...

.PHONY: test-race
test-race:
	go test -race ./...

# Acceptance tests run against a real cluster and registry. They cover what unit
# tests cannot: BuildKit talking to a registry, server-side apply, rollout gating,
# and the failure diagnostics.
#
#   export BUILDKIT_HOST=docker-container://buildkitd
#   export DEMO_SECRET=any-value
#   make acceptance
.PHONY: acceptance
acceptance: build
	./test/acceptance/run.sh

.PHONY: cover
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint:
	go vet ./...
	gofmt -l -d .

.PHONY: tidy
tidy:
	go mod tidy

# Cross-compile the binaries published to releases. CGO is off so each is a
# single static file with no runtime dependencies.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: release
release:
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building dist/$(BINARY)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch $(PKG) || exit 1; \
	done

.PHONY: clean
clean:
	rm -rf bin dist coverage.out
