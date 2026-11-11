# Tool versions. CI pins the same versions (.github/workflows); bump together.
GOLANGCI_LINT_VERSION   ?= v2.15.0
GOVULNCHECK_VERSION     ?= v1.9.0
ACTIONLINT_VERSION      ?= v1.7.12
ZIZMOR_VERSION          ?= 1.32.0
GH_ACTIONS_LOCK_VERSION ?= v0.4.1
FUZZTIME                ?= 30s
STRESS_COUNT            ?= 20

# `go run pkg@version` builds a tool with the toolchain its own go.mod asks for
# when the local go is older. Build the tools with the toolchain of this
# module's go.mod instead: golangci-lint refuses to lint code that targets a
# newer Go than the one it was built with.
GO_TOOLCHAIN  = $(shell go env GOVERSION)
GOLANGCI_LINT ?= GOTOOLCHAIN=$(GO_TOOLCHAIN) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
# uvx or pipx (preinstalled on GitHub-hosted runners) runs the pinned zizmor.
ZIZMOR ?= $(if $(shell command -v uvx 2>/dev/null),uvx zizmor@$(ZIZMOR_VERSION),pipx run zizmor==$(ZIZMOR_VERSION))
# gh-actions-lock (the `gh actions-lock` extension) runs through `go run` too.
GH_ACTIONS_LOCK ?= go run github.com/github/gh-actions-lock/cmd/gh-actions-lock@$(GH_ACTIONS_LOCK_VERSION)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -X github.com/yuuki/lustremesh/internal/cli.Version=$(VERSION)

.PHONY: build check test test-race test-stress test-fuzz vet lint fmt lint-actions lock-actions vuln clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o lustremesh ./cmd/lustremesh/

# Local pre-PR gate: what the CI jobs check, except the repetitions
# (test-stress) and fuzzing (test-fuzz). Needs no root and no Lustre: the
# tests run against fake parameter trees and start real worker processes.
check: lint lint-actions test-race build
	GOOS=darwin go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

# Repeat the tests that depend on timing: the watchdog, the locks, the
# supervisor and the holds.
test-stress:
	go test -race -count=$(STRESS_COUNT) ./internal/worker/ ./internal/supervisor/ ./internal/cli/

# Run every fuzz target for FUZZTIME each. A crasher is written to
# testdata/fuzz/<FuzzTarget>/ of its package.
test-fuzz:
	@set -e; for pkg in $$(go list ./...); do \
		targets=$$(go test -list '^Fuzz' $$pkg) || exit 1; \
		for f in $$targets; do \
			case $$f in Fuzz*) ;; *) continue ;; esac; \
			echo "== $$pkg $$f"; \
			go test -run='^$$' -fuzz="^$$f\$$" -fuzztime=$(FUZZTIME) $$pkg; \
		done; \
	done

vet:
	go vet ./...

# golangci-lint (linters and formatters of .golangci.yml) and go.mod tidiness.
lint:
	$(GOLANGCI_LINT) run
	go mod tidy -diff

# Apply the formatters of .golangci.yml.
fmt:
	$(GOLANGCI_LINT) fmt

# Lint (actionlint) and audit (zizmor) the GitHub Actions workflows. zizmor runs
# its online audits too when GH_TOKEN is set, as in CI.
# gh-actions-lock checks that .github/workflows/actions.lock pins every
# action the workflows use; with GH_TOKEN it also verifies each pin upstream.
lint-actions:
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	$(ZIZMOR) $(if $(GH_TOKEN),,--offline) .
	$(GH_ACTIONS_LOCK) $(if $(GH_TOKEN),--verify,--verify-local)

# Pin the actions in .github/workflows/actions.lock after a `uses:` changed,
# then review the diff. Needs a GitHub token (GH_TOKEN or `gh auth login`).
lock-actions:
	$(GH_ACTIONS_LOCK)

# Known vulnerabilities reachable from the code.
vuln:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

clean:
	rm -f lustremesh
