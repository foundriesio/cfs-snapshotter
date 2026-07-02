// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
)

func TestNewSnapshotterRejectsEmptyRoot(t *testing.T) {
	if _, err := NewSnapshotter(context.Background(), "", t.TempDir()); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("NewSnapshotter(%q) error = %v, want invalid argument", "", err)
	}
}

func TestNewSnapshotterAcceptsValidRoot(t *testing.T) {
	sn, err := NewSnapshotter(context.Background(), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	if sn == nil {
		t.Fatal("NewSnapshotter() returned a nil snapshotter")
	}
	if err := sn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// newTestSnapshotter returns a snapshotter over a fresh temp root and registers
// its Close with t.Cleanup.
func newTestSnapshotter(t *testing.T) snapshots.Snapshotter {
	t.Helper()
	return newTestSnapshotterWithEngine(t, &fakeEngine{})
}

func newTestSnapshotterWithEngine(t *testing.T, engine engineAPI) snapshots.Snapshotter {
	t.Helper()
	sn, err := newSnapshotter(context.Background(), t.TempDir(), engine)
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return sn
}

// TestLifecycle covers the active → committed transition and that Walk lists the
// committed snapshot.
func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)

	if _, err := sn.Prepare(ctx, "active", ""); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if info, err := sn.Stat(ctx, "active"); err != nil {
		t.Fatalf("Stat(active) error = %v", err)
	} else if info.Kind != snapshots.KindActive {
		t.Fatalf("Stat(active).Kind = %v, want %v", info.Kind, snapshots.KindActive)
	}

	if err := sn.Commit(ctx, "committed", "active"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if info, err := sn.Stat(ctx, "committed"); err != nil {
		t.Fatalf("Stat(committed) error = %v", err)
	} else if info.Kind != snapshots.KindCommitted {
		t.Fatalf("Stat(committed).Kind = %v, want %v", info.Kind, snapshots.KindCommitted)
	}
	// The active key no longer exists after commit.
	if _, err := sn.Stat(ctx, "active"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(active) after commit error = %v, want not found", err)
	}

	var walked []string
	if err := sn.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		walked = append(walked, info.Name)
		return nil
	}); err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	if len(walked) != 1 || walked[0] != "committed" {
		t.Fatalf("Walk() listed %v, want [committed]", walked)
	}

	viewMounts, err := sn.View(ctx, "view", "committed")
	if err != nil {
		t.Fatalf("View() error = %v", err)
	}
	if len(viewMounts) != 1 {
		t.Fatalf("View() returned %d mounts, want 1: %v", len(viewMounts), viewMounts)
	}
	if got := viewMounts[0].Options[len(viewMounts[0].Options)-1]; got != "ro" {
		t.Fatalf("View() mount ro flag = %q, want ro", got)
	}
	if err := sn.Remove(ctx, "view"); err != nil {
		t.Fatalf("Remove(view) error = %v", err)
	}
}

// TestParentResolution covers descending from a committed parent and the error
// contracts for an unknown parent and a duplicate key.
func TestParentResolution(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)

	if _, err := sn.Prepare(ctx, "base", ""); err != nil {
		t.Fatalf("Prepare(base) error = %v", err)
	}
	if err := sn.Commit(ctx, "base-committed", "base"); err != nil {
		t.Fatalf("Commit(base) error = %v", err)
	}

	if _, err := sn.Prepare(ctx, "child", "base-committed"); err != nil {
		t.Fatalf("Prepare(child, base-committed) error = %v", err)
	}

	if _, err := sn.Prepare(ctx, "orphan", "no-such-parent"); !errdefs.IsNotFound(err) {
		t.Fatalf("Prepare() with unknown parent error = %v, want not found", err)
	}

	if _, err := sn.Prepare(ctx, "child", ""); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Prepare() with duplicate key error = %v, want already exists", err)
	}
}

func TestPrepareTarget(t *testing.T) {
	ctx := context.Background()
	diffIDs := []digest.Digest{
		digest.FromString("target-layer-1"),
		digest.FromString("target-layer-2"),
	}
	targets := chainIDs(diffIDs...)
	sn := newTestSnapshotterWithEngine(t, fakeEngineForImages(testImageInfo(digest.FromString("target-root"), diffIDs...)))
	target1 := targets[0].String()
	target2 := targets[1].String()

	mounts, err := sn.Prepare(ctx, "extract-1", "", snapshots.WithLabels(map[string]string{
		targetSnapshotLabel: target1,
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Prepare(extract-1) error = %v, want already exists", err)
	}
	if mounts != nil {
		t.Fatalf("Prepare(extract-1) mounts = %v, want nil", mounts)
	}

	info, err := sn.Stat(ctx, target1)
	if err != nil {
		t.Fatalf("Stat(target1) error = %v", err)
	}
	if info.Kind != snapshots.KindCommitted {
		t.Fatalf("Stat(target1).Kind = %v, want %v", info.Kind, snapshots.KindCommitted)
	}
	if info.Parent != "" {
		t.Fatalf("Stat(target1).Parent = %q, want empty", info.Parent)
	}
	if got := info.Labels[targetSnapshotLabel]; got != target1 {
		t.Fatalf("Stat(target1).Labels[%q] = %q, want %q", targetSnapshotLabel, got, target1)
	}

	if _, err := sn.Prepare(ctx, "extract-2", target1, snapshots.WithLabels(map[string]string{
		targetSnapshotLabel: target2,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Prepare(extract-2) error = %v, want already exists", err)
	}
	info, err = sn.Stat(ctx, target2)
	if err != nil {
		t.Fatalf("Stat(target2) error = %v", err)
	}
	if info.Parent != target1 {
		t.Fatalf("Stat(target2).Parent = %q, want %q", info.Parent, target1)
	}

	filter := fmt.Sprintf(`labels."containerd.io/snapshot.ref"==%s,parent==%q`, target2, target1)
	var walked []snapshots.Info
	if err := sn.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		walked = append(walked, info)
		return nil
	}, filter); err != nil {
		t.Fatalf("Walk(%q) error = %v", filter, err)
	}
	if len(walked) != 1 {
		t.Fatalf("Walk(%q) yielded %d records, want 1: %v", filter, len(walked), walked)
	}
	if walked[0].Name != target2 {
		t.Fatalf("Walk(%q)[0].Name = %q, want %q", filter, walked[0].Name, target2)
	}

	if _, err := sn.Prepare(ctx, "extract-1b", "", snapshots.WithLabels(map[string]string{
		targetSnapshotLabel: target1,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Prepare(extract-1b) error = %v, want already exists", err)
	}
	if _, err := sn.Stat(ctx, "extract-1b"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(extract-1b) error = %v, want not found", err)
	}

	mounts, err = sn.Prepare(ctx, "active", target2)
	if err != nil {
		t.Fatalf("Prepare(active) error = %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("Prepare(active) returned %d mounts, want 1: %v", len(mounts), mounts)
	}
	if mounts[0].Type != mountType {
		t.Fatalf("Prepare(active) mount type = %q, want %q", mounts[0].Type, mountType)
	}
}

func TestPrepareTargetMissingChainFailsClosed(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotterWithEngine(t, fakeEngineForImages())
	target := digest.FromString("missing-target").String()

	mounts, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(map[string]string{
		targetSnapshotLabel: target,
	}))
	if !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("Prepare() error = %v, want failed precondition", err)
	}
	if mounts != nil {
		t.Fatalf("Prepare() mounts = %v, want nil", mounts)
	}
	if !strings.Contains(err.Error(), target) {
		t.Fatalf("Prepare() error %q does not name target %s", err, target)
	}
	if _, err := sn.Stat(ctx, target); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(target) error = %v, want not found", err)
	}
	if _, err := sn.Stat(ctx, "extract"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(extract) error = %v, want not found", err)
	}
}

// TestPrepareAppliesOptions covers that a successful snapshot option is recorded
// and a failing option surfaces its error.
func TestPrepareAppliesOptions(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)

	const labelKey = "containerd.io/snapshot/cfs.test"
	opt := snapshots.WithLabels(map[string]string{labelKey: "applied"})
	if _, err := sn.Prepare(ctx, "labeled", "", opt); err != nil {
		t.Fatalf("Prepare() with option error = %v", err)
	}
	info, err := sn.Stat(ctx, "labeled")
	if err != nil {
		t.Fatalf("Stat(labeled) error = %v", err)
	}
	if got := info.Labels[labelKey]; got != "applied" {
		t.Fatalf("Stat(labeled).Labels[%q] = %q, want %q", labelKey, got, "applied")
	}

	optErr := errors.New("boom")
	failing := func(*snapshots.Info) error { return optErr }
	if _, err := sn.Prepare(ctx, "bad", "", failing); !errors.Is(err, optErr) {
		t.Fatalf("Prepare() with failing option error = %v, want wrapped %v", err, optErr)
	}
	// The failed Prepare leaves no record behind.
	if _, err := sn.Stat(ctx, "bad"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(bad) error = %v, want not found", err)
	}
}

// TestUpdateRoundTrip covers writing and reading back a label via Update.
func TestUpdateRoundTrip(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)

	if _, err := sn.Prepare(ctx, "k", ""); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Commit(ctx, "n", "k"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	const labelKey = "containerd.io/snapshot/cfs.update"
	updated, err := sn.Update(ctx, snapshots.Info{
		Name:   "n",
		Labels: map[string]string{labelKey: "v"},
	}, "labels")
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got := updated.Labels[labelKey]; got != "v" {
		t.Fatalf("Update() returned Labels[%q] = %q, want %q", labelKey, got, "v")
	}
	info, err := sn.Stat(ctx, "n")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Labels[labelKey]; got != "v" {
		t.Fatalf("Stat() Labels[%q] = %q, want %q", labelKey, got, "v")
	}
}

// TestUsage covers that Usage returns without error for an active snapshot.
func TestUsage(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)

	if _, err := sn.Prepare(ctx, "k", ""); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := sn.Usage(ctx, "k"); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
}

// TestRemove covers that Remove drops the metadata record and that no snapshot
// directory is left behind.
func TestRemove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sn, err := NewSnapshotter(ctx, root, t.TempDir())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	if _, err := sn.Prepare(ctx, "k", ""); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Remove(ctx, "k"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := sn.Stat(ctx, "k"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat() after remove error = %v, want not found", err)
	}

	entries, err := os.ReadDir(root + "/snapshots")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("snapshots dir not empty after remove: %v", entries)
	}
}

// TestPersistence covers that committed snapshots survive reopening the
// snapshotter on the same root (metastore persistence).
func TestPersistence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	sn, err := NewSnapshotter(ctx, root, t.TempDir())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	if _, err := sn.Prepare(ctx, "active", ""); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Commit(ctx, "committed", "active"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := sn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := NewSnapshotter(ctx, root, t.TempDir())
	if err != nil {
		t.Fatalf("NewSnapshotter() reopen error = %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if info, err := reopened.Stat(ctx, "committed"); err != nil {
		t.Fatalf("Stat(committed) after reopen error = %v", err)
	} else if info.Kind != snapshots.KindCommitted {
		t.Fatalf("Stat(committed).Kind after reopen = %v, want %v", info.Kind, snapshots.KindCommitted)
	}
}
