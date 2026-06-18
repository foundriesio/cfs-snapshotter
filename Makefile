# SPDX-License-Identifier: Apache-2.0

GO ?= go
GOFLAGS ?=

# Build artifacts live under bin/.
BIN := bin
CMD := containerd-cfs-grpc
PKG := github.com/foundriesio/cfs-snapshotter

# Install location (overridable, e.g. DESTDIR for packaging).
PREFIX ?= /usr/local
DESTDIR ?=

# Version stamping: derive from git, fall back to "dev"/"none".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
LDFLAGS := -X $(PKG)/pkg/version.Version=$(VERSION) -X $(PKG)/pkg/version.Revision=$(REVISION)

.PHONY: all
all: fmt vet test build

# build compiles the snapshotter daemon into bin/.
.PHONY: build
build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$(CMD) ./cmd/$(CMD)

# install places the daemon under $(DESTDIR)$(PREFIX)/bin.
.PHONY: install
install: build
	install -D -m 0755 $(BIN)/$(CMD) $(DESTDIR)$(PREFIX)/bin/$(CMD)

# test runs the unit test suite.
.PHONY: test
test:
	$(GO) test $(GOFLAGS) ./...

# vet runs go vet across all packages.
.PHONY: vet
vet:
	$(GO) vet $(GOFLAGS) ./...

# fmt verifies that all Go sources are gofmt-clean; CI rejects unformatted code.
.PHONY: fmt
fmt:
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then \
		echo "gofmt needs to be run on:"; \
		echo "$$files"; \
		exit 1; \
	fi

# clean removes build artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN)
