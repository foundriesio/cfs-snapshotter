// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/testsuite"
	"github.com/containerd/containerd/v2/pkg/testutil"
)

// TestSnapshotterConformance runs containerd's reusable snapshotter
// conformance suite against a snapshotter backed by a real knarr store. The
// suite mounts, so it needs real root and is skipped unless -test.root is
// given: `make root-test`.
func TestSnapshotterConformance(t *testing.T) {
	testutil.RequiresRoot(t)
	testsuite.SnapshotterSuite(t, "cfs", newConformanceSnapshotter)
}

// newConformanceSnapshotter builds the snapshotter the conformance suite
// drives, with a knarr store next to the root the suite hands out.
func newConformanceSnapshotter(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
	sn, err := NewSnapshotter(ctx, Config{
		Root:      root,
		KnarrRoot: filepath.Join(filepath.Dir(root), "knarr"),
	})
	if err != nil {
		return nil, nil, err
	}
	return sn, sn.Close, nil
}
