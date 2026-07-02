// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/foundriesio/knarr/pkg/knarr"
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
	// An empty store resolves every parent chain to "not materialized", so
	// these lifecycle snapshots take the placeholder-bind path.
	return newTestSnapshotterWithEngine(t, fakeEngineForImages())
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

	// An unlabeled scratch Prepare is unaffected by the target path and gets a
	// placeholder bind of its own directory.
	mounts, err = sn.Prepare(ctx, "active", "")
	if err != nil {
		t.Fatalf("Prepare(active) error = %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("Prepare(active) returned %d mounts, want 1: %v", len(mounts), mounts)
	}
	if mounts[0].Type != mountType {
		t.Fatalf("Prepare(active) mount type = %q, want %q", mounts[0].Type, mountType)
	}
	if filepath.Base(mounts[0].Source) == "fs" {
		t.Fatalf("Prepare(active) source = %q, want the snapshot dir, not an fs mount", mounts[0].Source)
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

// TestPrepareOverImageReturnsWritableOverlay covers that an active snapshot
// over a materialized image mounts the image's metadata read-only at the
// chain snapshot's fs directory and returns a single writable overlay mount
// spec for the consumer to execute, with the knarr object directory as the
// data-only lower and verity required.
func TestPrepareOverImageReturnsWritableOverlay(t *testing.T) {
	ctx := context.Background()
	sn, engine, root, leaf := newBackedSnapshotter(t, notMounted)
	var calls []mountCall
	recordMounts(engine, &calls)

	mounts, err := sn.Prepare(ctx, "container", leaf)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("engine Mount calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.ref != root.String() {
		t.Fatalf("mount ref = %s, want %s", c.ref, root)
	}
	if filepath.Base(c.target) != "fs" {
		t.Fatalf("mount target = %s, want a .../fs directory", c.target)
	}
	if c.opts.UpperDir != "" || c.opts.WorkDir != "" {
		t.Fatalf("image mount set overlay dirs: %+v, want read-only", c.opts)
	}
	if !c.opts.MetadataOnly {
		t.Fatalf("image mount opts = %+v, want MetadataOnly", c.opts)
	}
	id, _, err := sn.snapshotInfo(ctx, "container")
	if err != nil {
		t.Fatalf("snapshotInfo(container) error = %v", err)
	}
	dir := sn.getSnapshotDir(id)
	want := []string{
		"workdir=" + filepath.Join(dir, "work"),
		"upperdir=" + filepath.Join(dir, "upper"),
		"lowerdir=" + c.target + "::" + testObjectsDir,
		"redirect_dir=on",
		"metacopy=on",
		"verity=require",
	}
	if len(mounts) != 1 || mounts[0].Type != "overlay" {
		t.Fatalf("mounts = %v, want a single overlay spec", mounts)
	}
	if !slices.Equal(mounts[0].Options, want) {
		t.Fatalf("overlay options = %v, want %v", mounts[0].Options, want)
	}
}

// TestViewOverImageReturnsReadOnlyOverlay covers that a view over a
// materialized image with no intermediate layers is a read-only flat overlay:
// the metadata mount alone carries no file data, so even a plain view needs
// the object directory attached as its data-only lower.
func TestViewOverImageReturnsReadOnlyOverlay(t *testing.T) {
	ctx := context.Background()
	sn, engine, root, leaf := newBackedSnapshotter(t, notMounted)
	var calls []mountCall
	recordMounts(engine, &calls)

	mounts, err := sn.View(ctx, "view", leaf)
	if err != nil {
		t.Fatalf("View() error = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("engine Mount calls = %d, want 1", len(calls))
	}
	if calls[0].ref != root.String() {
		t.Fatalf("mount ref = %s, want %s", calls[0].ref, root)
	}
	if !calls[0].opts.MetadataOnly {
		t.Fatalf("image mount opts = %+v, want MetadataOnly", calls[0].opts)
	}
	want := []string{
		"lowerdir=" + calls[0].target + "::" + testObjectsDir,
		"redirect_dir=on",
		"metacopy=on",
		"verity=require",
	}
	if len(mounts) != 1 || mounts[0].Type != "overlay" {
		t.Fatalf("mounts = %v, want a single overlay spec", mounts)
	}
	if !slices.Equal(mounts[0].Options, want) {
		t.Fatalf("overlay options = %v, want %v", mounts[0].Options, want)
	}
}

// TestMountsReusesExistingMount covers that a second mounts request for an
// already-mounted snapshot does not mount the image again.
func TestMountsReusesExistingMount(t *testing.T) {
	ctx := context.Background()
	sn, engine, _, leaf := newBackedSnapshotter(t, alwaysMounted)
	var calls []mountCall
	recordMounts(engine, &calls)

	if _, err := sn.Prepare(ctx, "container", leaf); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := sn.Mounts(ctx, "container"); err != nil {
		t.Fatalf("Mounts() error = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("engine Mount calls = %d, want 0 for an already-mounted snapshot", len(calls))
	}
}

// TestPrepareRejectsIntermediateChain covers that a snapshot cannot be layered
// over an intermediate layer chain that is not a full image.
func TestPrepareRejectsIntermediateChain(t *testing.T) {
	ctx := context.Background()
	diffIDs := []digest.Digest{digest.FromString("l1"), digest.FromString("l2")}
	engine := fakeEngineForImages(testImageInfo(digest.FromString("img-root"), diffIDs...))
	intermediate := chainIDs(diffIDs...)[0].String()
	sn := newSnapshotterForTest(t, engine, notMounted)

	if _, err := sn.Prepare(ctx, "container", intermediate); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("Prepare(intermediate) error = %v, want invalid argument", err)
	}
	if _, err := sn.Stat(ctx, "container"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(container) error = %v, want not found", err)
	}
}

// TestPrepareOverCommittedInitLayer covers the flow docker uses to create a
// container: an init snapshot prepared over the image chain, committed, and a
// container snapshot prepared over the committed init layer. The container's
// overlay spec must stack the init layer's upper directory over the image's
// shared composefs mount.
func TestPrepareOverCommittedInitLayer(t *testing.T) {
	ctx := context.Background()
	sn, engine, _, leaf := newBackedSnapshotter(t, notMounted)
	var calls []mountCall
	recordMounts(engine, &calls)

	if _, err := sn.Prepare(ctx, "init-key", leaf); err != nil {
		t.Fatalf("Prepare(init-key) error = %v", err)
	}
	if err := sn.Commit(ctx, "init", "init-key"); err != nil {
		t.Fatalf("Commit(init) error = %v", err)
	}
	initID, _, err := sn.snapshotInfo(ctx, "init")
	if err != nil {
		t.Fatalf("snapshotInfo(init) error = %v", err)
	}
	initUpper := filepath.Join(sn.getSnapshotDir(initID), "upper")

	calls = nil
	mounts, err := sn.Prepare(ctx, "container", "init")
	if err != nil {
		t.Fatalf("Prepare(container) error = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("engine Mount calls = %d, want 1 (shared composefs mount)", len(calls))
	}
	chainFS := calls[0].target
	id, _, err := sn.snapshotInfo(ctx, "container")
	if err != nil {
		t.Fatalf("snapshotInfo(container) error = %v", err)
	}
	dir := sn.getSnapshotDir(id)
	want := []string{
		"workdir=" + filepath.Join(dir, "work"),
		"upperdir=" + filepath.Join(dir, "upper"),
		"lowerdir=" + initUpper + ":" + chainFS + "::" + testObjectsDir,
		"redirect_dir=on",
		"metacopy=on",
		"verity=require",
	}
	if len(mounts) != 1 || mounts[0].Type != "overlay" || !slices.Equal(mounts[0].Options, want) {
		t.Fatalf("mounts = %v, want overlay with options %v", mounts, want)
	}

	// Mounts() must reproduce the same spec (and re-ensure the image
	// mount) for recovery.
	calls = nil
	recovered, err := sn.Mounts(ctx, "container")
	if err != nil {
		t.Fatalf("Mounts(container) error = %v", err)
	}
	if len(calls) != 1 || !slices.Equal(recovered[0].Options, want) {
		t.Fatalf("recovered mounts = %v (composefs mounts %d), want options %v", recovered, len(calls), want)
	}
}

// TestRemoveUnmountsChainSnapshot covers that removing the chain snapshot
// carrying the shared composefs mount tears that mount down first.
func TestRemoveUnmountsChainSnapshot(t *testing.T) {
	ctx := context.Background()
	sn, engine, _, leaf := newBackedSnapshotter(t, alwaysMounted)
	var unmounted []string
	engine.unmountFunc = func(_ context.Context, target string) error {
		unmounted = append(unmounted, target)
		return nil
	}

	if err := sn.Remove(ctx, leaf); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if len(unmounted) != 1 || filepath.Base(unmounted[0]) != "fs" {
		t.Fatalf("Unmount targets = %v, want one .../fs", unmounted)
	}
	if _, err := sn.Stat(ctx, leaf); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat() after remove = %v, want not found", err)
	}
}

// TestRemoveKeepsMountWhileChildrenExist covers that removing a chain snapshot
// that still has children fails without touching the shared composefs mount
// their overlay specs reference.
func TestRemoveKeepsMountWhileChildrenExist(t *testing.T) {
	ctx := context.Background()
	sn, engine, _, leaf := newBackedSnapshotter(t, alwaysMounted)
	var unmounted []string
	engine.unmountFunc = func(_ context.Context, target string) error {
		unmounted = append(unmounted, target)
		return nil
	}

	if _, err := sn.Prepare(ctx, "container", leaf); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Remove(ctx, leaf); err == nil {
		t.Fatal("Remove(leaf with child) error = nil, want failure")
	}
	if len(unmounted) != 0 {
		t.Fatalf("Unmount targets = %v, want none while children exist", unmounted)
	}
	if _, err := sn.Stat(ctx, leaf); err != nil {
		t.Fatalf("Stat(leaf) after failed remove = %v, want the record preserved", err)
	}
}

// TestPrepareRollsBackOnMountFailure covers that a Prepare whose composefs
// mount fails removes the snapshot record it created, so a retry of the same
// key does not fail with already-exists.
func TestPrepareRollsBackOnMountFailure(t *testing.T) {
	ctx := context.Background()
	sn, engine, _, leaf := newBackedSnapshotter(t, notMounted)
	engine.mountFunc = func(context.Context, string, string, ...knarr.MountOption) error {
		return errors.New("mount.composefs failed")
	}

	if _, err := sn.Prepare(ctx, "container", leaf); err == nil {
		t.Fatal("Prepare() error = nil, want mount failure")
	}
	if _, err := sn.Stat(ctx, "container"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(container) after failed prepare = %v, want not found", err)
	}
	// A retry with the same key succeeds once the mount works again.
	engine.mountFunc = func(context.Context, string, string, ...knarr.MountOption) error { return nil }
	if _, err := sn.Prepare(ctx, "container", leaf); err != nil {
		t.Fatalf("Prepare() retry error = %v", err)
	}
}

// TestRemoveAbortsWhenUnmountFails covers that a failed unmount aborts the
// remove and leaves the snapshot record in place.
func TestRemoveAbortsWhenUnmountFails(t *testing.T) {
	ctx := context.Background()
	sn, engine, _, leaf := newBackedSnapshotter(t, alwaysMounted)
	engine.unmountFunc = func(context.Context, string) error {
		return errors.New("device busy")
	}

	if err := sn.Remove(ctx, leaf); err == nil {
		t.Fatal("Remove() error = nil, want failure when unmount fails")
	}
	if _, err := sn.Stat(ctx, leaf); err != nil {
		t.Fatalf("Stat() after failed remove = %v, want the record preserved", err)
	}
}

// TestMountsFailsWhenBackingImageRemoved covers that recovering the mounts of a
// snapshot whose backing image is gone fails rather than binding the snapshot
// directory as the root filesystem.
func TestMountsFailsWhenBackingImageRemoved(t *testing.T) {
	ctx := context.Background()
	sn, engine, _, leaf := newBackedSnapshotter(t, notMounted)
	engine.mountFunc = func(context.Context, string, string, ...knarr.MountOption) error { return nil }
	if _, err := sn.Prepare(ctx, "container", leaf); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	// The backing image disappears from the store.
	engine.listFunc = func(context.Context) ([]knarr.Image, error) { return nil, nil }
	engine.inspectFunc = func(_ context.Context, ref string) (*knarr.ImageInfo, error) {
		return nil, fmt.Errorf("removed %s: %w", ref, knarr.ErrNotFound)
	}
	if _, err := sn.Mounts(ctx, "container"); !errdefs.IsNotFound(err) {
		t.Fatalf("Mounts() after image removal = %v, want not found", err)
	}
}

type mountCall struct {
	ref    string
	target string
	opts   knarr.MountOptions
}

func newSnapshotterForTest(t *testing.T, engine *fakeEngine, mounted func(string) (bool, error)) *snapshotter {
	t.Helper()
	sn, err := newSnapshotter(context.Background(), t.TempDir(), engine)
	if err != nil {
		t.Fatalf("newSnapshotter() error = %v", err)
	}
	sn.mounted = mounted
	t.Cleanup(func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return sn
}

// newBackedSnapshotter builds a snapshotter whose knarr store and metadata both
// hold one two-layer image: the committed layer chain is seeded exactly as the
// fast-path target prepares would during a pull. It returns the snapshotter, its
// engine, the image root, and the leaf chain id a container is layered over.
func newBackedSnapshotter(t *testing.T, mounted func(string) (bool, error)) (*snapshotter, *fakeEngine, digest.Digest, string) {
	t.Helper()
	ctx := context.Background()
	diffIDs := []digest.Digest{digest.FromString("l1"), digest.FromString("l2")}
	root := digest.FromString("img-root")
	engine := fakeEngineForImages(testImageInfo(root, diffIDs...))
	sn := newSnapshotterForTest(t, engine, mounted)

	parent := ""
	for i, chainID := range chainIDs(diffIDs...) {
		_, err := sn.Prepare(ctx, fmt.Sprintf("seed-%d", i), parent, snapshots.WithLabels(map[string]string{
			targetSnapshotLabel: chainID.String(),
		}))
		if !errdefs.IsAlreadyExists(err) {
			t.Fatalf("seed target %s error = %v, want already exists", chainID, err)
		}
		parent = chainID.String()
	}
	return sn, engine, root, parent
}

func recordMounts(engine *fakeEngine, calls *[]mountCall) {
	engine.mountFunc = func(_ context.Context, ref, target string, opts ...knarr.MountOption) error {
		var mo knarr.MountOptions
		for _, o := range opts {
			o(&mo)
		}
		*calls = append(*calls, mountCall{ref: ref, target: target, opts: mo})
		return nil
	}
}

func notMounted(string) (bool, error)    { return false, nil }
func alwaysMounted(string) (bool, error) { return true, nil }
