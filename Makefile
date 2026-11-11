# Tool versions; bump together with CI once the repository has workflows.
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.9.0
FUZZTIME              ?= 30s

# `go run pkg@version` builds a tool with the toolchain its own go.mod asks for
# when the local go is older. Build the tools with the toolchain of this
# module's go.mod instead: golangci-lint refuses to lint code that targets a
# newer Go than the one it was built with.
GO_TOOLCHAIN  = $(shell go env GOVERSION)
GOLANGCI_LINT ?= GOTOOLCHAIN=$(GO_TOOLCHAIN) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -X github.com/yuuki/lustremesh/internal/cli.Version=$(VERSION)

.PHONY: build check test test-race test-fuzz vet lint fmt vuln clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o lustremesh ./cmd/lustremesh/

# Local pre-PR gate. Needs no root and no Lustre: the tests run against fake
# parameter trees and start real worker processes.
check: lint test-race build
	GOOS=darwin go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

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

# Known vulnerabilities reachable from the code.
vuln:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

clean:
	rm -f lustremesh
