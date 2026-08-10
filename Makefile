# SPDX-License-Identifier: Apache-2.0

GO ?= go
GOFLAGS ?=
GOLANGCI_LINT ?= golangci-lint

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
LDFLAGS := -s -w -X $(PKG)/pkg/version.Version=$(VERSION) -X $(PKG)/pkg/version.Revision=$(REVISION)

.PHONY: all
all: fmt vet lint test build

# build compiles the snapshotter daemon into bin/. cgo is disabled so the
# binary is fully static and runs on any Linux of the right architecture.
.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/$(CMD) ./cmd/$(CMD)

# install places the daemon under $(DESTDIR)$(PREFIX)/bin.
.PHONY: install
install: build
	install -D -m 0755 $(BIN)/$(CMD) $(DESTDIR)$(PREFIX)/bin/$(CMD)

# test runs the unit test suite.
.PHONY: test
test:
	$(GO) test $(GOFLAGS) ./...

# root-test runs containerd's snapshotter conformance suite. It mounts, so it
# needs real root; without -test.root the suite is skipped.
.PHONY: root-test
root-test:
	$(GO) test $(GOFLAGS) -v -run TestSnapshotterConformance ./internal/snapshot -test.root

# test-e2e runs the composefs data-plane test from inside the devcontainer.
.PHONY: test-e2e
test-e2e: run-cfs
	.devcontainer/scripts/e2e-fastpath.sh

# run-cfs builds and starts the cfs proxy snapshotter in the devcontainer.
.PHONY: run-cfs
run-cfs:
	.devcontainer/scripts/run-cfs.sh

# vet runs go vet across all packages.
.PHONY: vet
vet:
	$(GO) vet $(GOFLAGS) ./...

# lint runs golangci-lint (config in .golangci.yml).
.PHONY: lint
lint: fmt
	$(GOLANGCI_LINT) run

# fmt verifies that all Go sources are gofmt-clean; CI rejects unformatted
# code. Scoped to the source trees so build artifacts under bin/ (e.g. the
# deb target's vendored modules) are not scanned.
.PHONY: fmt
fmt:
	@files=$$(gofmt -l cmd internal pkg); \
	if [ -n "$$files" ]; then \
		echo "gofmt needs to be run on:"; \
		echo "$$files"; \
		exit 1; \
	fi

# clean removes build artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN)

# --- Debian packaging ---------------------------------------------------
#
# The .deb is built by debuild inside a local container image (debian/
# Dockerfile) so no Debian toolchain is needed on the host. The knarr
# dependency is not fetchable from inside that container, so the deb target
# vendors the module tree on the host first and the packaging build runs
# fully offline. The Debian version comes from the release tag (v0.1.0 ->
# 0.1.0, v0.1.0-rc.1 -> 0.1.0~rc.1 so finals sort after candidates); an
# untagged checkout gets a never-releasable snapshot version instead.

DEB_IMAGE ?= cfs-snapshotter-debuild:trixie
DEB_OUT ?= $(BIN)/deb
# Dot-prefixed so go tooling (./... patterns, gofmt) never scans the
# vendored upstream sources as part of this module.
DEB_VENDOR := $(BIN)/.deb-vendor

RELEASE_TAG := $(shell git describe --tags --exact-match --match 'v*' 2>/dev/null)
ifeq ($(RELEASE_TAG),)
DEB_VERSION ?= 0.0.0~snap.g$(shell git rev-parse --short HEAD 2>/dev/null || echo none)
else
DEB_VERSION ?= $(subst -rc.,~rc.,$(patsubst v%,%,$(RELEASE_TAG)))
endif

# deb-image builds the packaging container image.
.PHONY: deb-image
deb-image:
	docker build -t $(DEB_IMAGE) debian

# deb builds the .deb (plus .changes/.buildinfo) into $(DEB_OUT). VERSION,
# REVISION and DEB_VERSION are passed in explicitly: inside the container the
# git-derived defaults would silently fall back to "dev".
.PHONY: deb
deb: deb-image
	mkdir -p $(DEB_OUT)
	rm -rf $(DEB_VENDOR)
	GOPRIVATE=github.com/foundriesio $(GO) mod vendor -o $(DEB_VENDOR)
	docker run --rm \
		-v "$(CURDIR)":/src:ro \
		-v "$(CURDIR)/$(DEB_VENDOR)":/vendor:ro \
		-v "$(CURDIR)/$(DEB_OUT)":/out \
		-e VERSION=$(VERSION) -e REVISION=$(REVISION) -e DEB_VERSION=$(DEB_VERSION) \
		$(DEB_IMAGE) /src/debian/build-in-docker.sh

# deb-lint runs lintian over the built package.
.PHONY: deb-lint
deb-lint:
	docker run --rm -u "$$(id -u):$$(id -g)" \
		-v "$(CURDIR)/$(DEB_OUT)":/out:ro $(DEB_IMAGE) \
		sh -c 'lintian -I /out/*.changes'

# deb-test install-tests the .deb on clean Debian and Ubuntu containers:
# control fields, install, stamped version, unit and env file, reinstall,
# remove, purge.
.PHONY: deb-test
deb-test:
	DEB_VERSION=$(DEB_VERSION) VERSION=$(VERSION) ./scripts/deb-test.sh $(DEB_OUT)
