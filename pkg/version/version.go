// SPDX-License-Identifier: Apache-2.0

// Package version exposes the build version of cfs-snapshotter. The values are
// injected at build time via -ldflags (see the Makefile); they default to
// "dev"/"none" for `go build`/`go run` without those flags.
package version

import "fmt"

var (
	// Version is the released version, e.g. "v0.1.0" or a `git describe` string.
	Version = "dev"

	// Revision is the git commit the binary was built from.
	Revision = "none"
)

// String returns a human-readable "<version> (<revision>)" identifier.
func String() string {
	return fmt.Sprintf("%s (%s)", Version, Revision)
}
