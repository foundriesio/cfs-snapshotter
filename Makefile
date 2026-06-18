# SPDX-License-Identifier: Apache-2.0

GO ?= go
GOFLAGS ?=

# Build artifacts live under bin/ (added with the binary in a later commit).
BIN := bin

.PHONY: all
all: fmt vet test

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
