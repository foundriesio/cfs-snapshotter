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

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/errdefs"
	"github.com/foundriesio/knarr/pkg/knarr"
	"github.com/opencontainers/go-digest"
)

func TestOptFieldsExposesLabels(t *testing.T) {
	fields := optFields([]snapshots.Opt{snapshots.WithLabels(map[string]string{
		"containerd.io/snapshot.ref": "sha256:abc",
	})})
	labels, ok := fields["labels"].(map[string]string)
	if !ok || labels["containerd.io/snapshot.ref"] != "sha256:abc" {
		t.Fatalf("optFields labels = %#v, want the attached label", fields["labels"])
	}
	if len(optFields(nil)) != 0 {
		t.Fatalf("optFields(nil) = %#v, want empty", optFields(nil))
	}
}

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

func TestNewSnapshotterRejectsMountOptionSeparators(t *testing.T) {
	ctx := context.Background()
	for _, sep := range []string{":", ","} {
		t.Run("root "+sep, func(t *testing.T) {
			engine := &fakeEngine{objectsDir: t.TempDir()}
			if _, err := newSnapshotter(ctx, t.TempDir()+sep+"root", engine); !errdefs.IsInvalidArgument(err) {
				t.Fatalf("newSnapshotter() root error = %v, want invalid argument", err)
			}
		})
		t.Run("objects "+sep, func(t *testing.T) {
			engine := &fakeEngine{objectsDir: t.TempDir() + sep + "objects"}
			if _, err := newSnapshotter(ctx, t.TempDir(), engine); !errdefs.IsInvalidArgument(err) {
				t.Fatalf("newSnapshotter() objects error = %v, want invalid argument", err)
			}
		})
	}
}

func TestIsMountedTreatsMissingPathAsNotMounted(t *testing.T) {
	sn, err := newSnapshotter(context.Background(), t.TempDir(), &fakeEngine{objectsDir: t.TempDir()})
	if err != nil {
		t.Fatalf("newSnapshotter() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	sn.mounted = func(string) (bool, error) {
		return false, fmt.Errorf("stat mount target: %w", os.ErrNotExist)
	}

	mounted, err := sn.isMounted("/missing")
	if err != nil {
		t.Fatalf("isMounted() error = %v", err)
	}
	if mounted {
		t.Fatal("isMounted() = true, want false")
	}
}

// newTestSnapshotter returns a snapshotter over a fresh temp root and registers
// its Close with t.Cleanup.
func newTestSnapshotter(t *testing.T) *snapshotter {
	t.Helper()
	return newSnapshotterForTest(t, fakeEngineForImages(), notMounted)
}

// imageChain commits an intermediate virtual snapshot named name, the way a
// pull's unpack handshake would, and makes the same chain available from the
// fake knarr store.
func imageChain(t *testing.T, ctx context.Context, sn *snapshotter, name string) {
	t.Helper()
	addFakeImage(sn.engine.(*fakeEngine), testImageInfo("root-"+name, digest.Digest(name)))
	if _, err := sn.Prepare(ctx, "extract-"+name, "", snapshots.WithLabels(map[string]string{
		"containerd.io/snapshot.ref": name,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("image chain handshake error = %v, want already exists", err)
	}
}

// TestLifecycle covers the active → committed transition and that Walk lists the
// committed snapshots.
func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)
	imageChain(t, ctx, sn, "img")

	if _, err := sn.Prepare(ctx, "active", "img"); err != nil {
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
	want := map[string]bool{"img": true, "committed": true}
	if len(walked) != len(want) {
		t.Fatalf("Walk() listed %v, want the chain and the committed snapshot", walked)
	}
	for _, name := range walked {
		if !want[name] {
			t.Fatalf("Walk() listed unexpected %q", name)
		}
	}
}

// TestParentResolution covers descending from a committed parent and the error
// contracts for an unknown parent and a duplicate key.
func TestParentResolution(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)
	imageChain(t, ctx, sn, "img")

	if _, err := sn.Prepare(ctx, "base", "img"); err != nil {
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

// TestPrepareAppliesOptions covers that a successful snapshot option is recorded
// and a failing option surfaces its error.
func TestPrepareAppliesOptions(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)

	imageChain(t, ctx, sn, "img")
	const labelKey = "containerd.io/snapshot/cfs.test"
	opt := snapshots.WithLabels(map[string]string{labelKey: "applied"})
	if _, err := sn.Prepare(ctx, "labeled", "img", opt); err != nil {
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
	imageChain(t, ctx, sn, "img")

	if _, err := sn.Prepare(ctx, "k", "img"); err != nil {
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
	imageChain(t, ctx, sn, "img")

	if _, err := sn.Prepare(ctx, "k", "img"); err != nil {
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
	engine := fakeEngineForImages()
	engine.objectsDir = t.TempDir()
	sn, err := newSnapshotter(ctx, root, engine)
	if err != nil {
		t.Fatalf("newSnapshotter() error = %v", err)
	}
	sn.mounted = notMounted
	t.Cleanup(func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	imageChain(t, ctx, sn, "img")
	if _, err := sn.Prepare(ctx, "k", "img"); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Remove(ctx, "k"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := sn.Stat(ctx, "k"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat() after remove error = %v, want not found", err)
	}
	// The chain's mount target belongs to the chain record and survives its
	// child; removing the chain drops it, children first as the garbage
	// collector would.
	if err := sn.Remove(ctx, "img"); err != nil {
		t.Fatalf("Remove(img) error = %v", err)
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
	engine := fakeEngineForImages()
	engine.objectsDir = t.TempDir()

	sn, err := newSnapshotter(ctx, root, engine)
	if err != nil {
		t.Fatalf("newSnapshotter() error = %v", err)
	}
	sn.mounted = notMounted
	imageChain(t, ctx, sn, "img")
	if _, err := sn.Prepare(ctx, "active", "img"); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Commit(ctx, "committed", "active"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := sn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := newSnapshotter(ctx, root, engine)
	if err != nil {
		t.Fatalf("newSnapshotter() reopen error = %v", err)
	}
	reopened.mounted = notMounted
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

// TestPrepareIntermediateSnapshotAnswersAlreadyExists exercises the unpack
// handshake: a Prepare carrying the snapshot.ref label must be answered with
// ErrAlreadyExists after committing an intermediate virtual snapshot under
// the requested name with the caller's labels, a repeat handshake must not
// leave an orphan active snapshot behind, and a Prepare without the label
// must keep taking the normal mount path.
func TestPrepareIntermediateSnapshotAnswersAlreadyExists(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	engine := fakeEngineForImages()
	engine.objectsDir = t.TempDir()
	sn, err := newSnapshotter(ctx, root, engine)
	if err != nil {
		t.Fatalf("newSnapshotter() error = %v", err)
	}
	sn.mounted = notMounted
	defer func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	const name = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	addFakeImage(engine, testImageInfo("handshake-root", digest.Digest(name)))
	labels := map[string]string{
		"containerd.io/snapshot.ref": name,
		"other":                      "kept",
	}
	if _, err := sn.Prepare(ctx, "extract-1 "+name, "", snapshots.WithLabels(labels)); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("handshake Prepare error = %v, want already exists", err)
	}
	info, err := sn.Stat(ctx, name)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", name, err)
	}
	if info.Kind != snapshots.KindCommitted || info.Labels["other"] != "kept" {
		t.Fatalf("intermediate snapshot info = %+v, want committed with caller labels", info)
	}

	// A repeat handshake for the same name must also answer already-exists
	// and must not leak its temporary active snapshot.
	if _, err := sn.Prepare(ctx, "extract-2 "+name, "", snapshots.WithLabels(labels)); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("repeat handshake Prepare error = %v, want already exists", err)
	}
	if _, err := sn.Stat(ctx, "extract-2 "+name); !errdefs.IsNotFound(err) {
		t.Fatalf("superseded active snapshot error = %v, want not found", err)
	}

	// Handshake records are metadata-only: no snapshot directory may have
	// been provisioned for them.
	entries, err := os.ReadDir(root + "/snapshots")
	if err != nil {
		t.Fatalf("read snapshots dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("snapshots dir has %d entries after handshakes, want none", len(entries))
	}

	// Without the label Prepare must take the normal mount path.
	mounts, err := sn.Prepare(ctx, "container-1", name)
	if err != nil {
		t.Fatalf("plain Prepare error = %v", err)
	}
	if len(mounts) == 0 {
		t.Fatal("plain Prepare returned no mounts")
	}
}

func TestPrepareOverImageReturnsWritableOverlay(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, notMounted)
	var calls []mountCall
	recordMounts(engine, &calls)

	mounts, err := sn.Prepare(ctx, "container", leaf)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("engine MountChain calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.chainID != leaf || !call.opts.MetadataOnly {
		t.Fatalf("MountChain() = chain %q opts %+v, want %q metadata-only", call.chainID, call.opts, leaf)
	}
	if fi, err := os.Stat(call.target); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("chain mount target mode = %v, %v; want 0755", fi, err)
	}
	id, _, err := sn.snapshotInfo(ctx, "container")
	if err != nil {
		t.Fatalf("snapshotInfo(container) error = %v", err)
	}
	dir := sn.getSnapshotDir(id)
	want := []string{
		"workdir=" + filepath.Join(dir, "work"),
		"upperdir=" + filepath.Join(dir, "upper"),
		"lowerdir=" + call.target + "::" + engine.objectsDir,
		"redirect_dir=on",
		"metacopy=on",
		"verity=require",
	}
	if len(mounts) != 1 || mounts[0].Type != "overlay" || !slices.Equal(mounts[0].Options, want) {
		t.Fatalf("mounts = %v, want overlay with options %v", mounts, want)
	}
}

func TestViewOverImageHasNoUpper(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, notMounted)
	var calls []mountCall
	recordMounts(engine, &calls)

	mounts, err := sn.View(ctx, "view", leaf)
	if err != nil {
		t.Fatalf("View() error = %v", err)
	}
	want := []string{
		"lowerdir=" + calls[0].target + "::" + engine.objectsDir,
		"redirect_dir=on",
		"metacopy=on",
		"verity=require",
	}
	if len(mounts) != 1 || !slices.Equal(mounts[0].Options, want) {
		t.Fatalf("View() mounts = %v, want options %v", mounts, want)
	}
	id, _, err := sn.snapshotInfo(ctx, "view")
	if err != nil {
		t.Fatalf("snapshotInfo(view) error = %v", err)
	}
	dir := sn.getSnapshotDir(id)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("view directory: %v", err)
	}
	for _, name := range []string{"upper", "work"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("view %s error = %v, want not exist", name, err)
		}
	}
}

func TestMountsReusesExistingChainMountWithoutStoreScan(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, notMounted)
	mounted := false
	sn.mounted = func(string) (bool, error) { return mounted, nil }
	var mountCalls int
	engine.mountChainFunc = func(context.Context, string, string, ...knarr.MountOption) error {
		mountCalls++
		mounted = true
		return nil
	}
	if _, err := sn.Prepare(ctx, "container", leaf); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if _, err := sn.Mounts(ctx, "container"); err != nil {
		t.Fatalf("Mounts() error = %v", err)
	}
	if mountCalls != 1 {
		t.Fatalf("MountChain calls = %d, want 1", mountCalls)
	}
}

func TestPrepareRejectsPrefixOnlyChain(t *testing.T) {
	ctx := context.Background()
	chain := digest.FromString("base-chain")
	derived := testImageInfo("derived", chain, digest.FromString("leaf-chain"))
	sn := newSnapshotterForTest(t, fakeEngineForImages(derived), notMounted)
	seedChainRecord(t, ctx, sn, chain.String(), "")

	if _, err := sn.Prepare(ctx, "container", chain.String()); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("Prepare() error = %v, want failed precondition", err)
	}
	if _, err := sn.Stat(ctx, "container"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(container) error = %v, want not found", err)
	}
}

func TestMountsComposeCommittedUpper(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, notMounted)
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
	if got, want := optionValue(t, mounts[0], "lowerdir"), initUpper+":"+calls[0].target+"::"+engine.objectsDir; got != want {
		t.Fatalf("lowerdir = %q, want %q", got, want)
	}
	calls = nil
	if _, err := sn.Mounts(ctx, "container"); err != nil {
		t.Fatalf("Mounts(container) error = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("Mounts() issued %d image mounts, want 1", len(calls))
	}
}

func TestMountsStopAtNearestImageChain(t *testing.T) {
	ctx := context.Background()
	chainA := digest.FromString("chain-a")
	chainB := digest.FromString("chain-b")
	engine := fakeEngineForImages(testImageInfo("a", chainA), testImageInfo("b", chainB))
	sn := newSnapshotterForTest(t, engine, notMounted)
	seedChainRecord(t, ctx, sn, chainA.String(), "")
	seedChainRecord(t, ctx, sn, chainB.String(), chainA.String())
	var calls []mountCall
	recordMounts(engine, &calls)

	mounts, err := sn.Prepare(ctx, "container", chainB.String())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(calls) != 1 || calls[0].chainID != chainB.String() {
		t.Fatalf("MountChain calls = %+v, want only image B", calls)
	}
	if got := optionValue(t, mounts[0], "lowerdir"); got != calls[0].target+"::"+engine.objectsDir {
		t.Fatalf("lowerdir = %q, want image B only", got)
	}
}

func TestPrepareWithoutImageAncestorFailsClosed(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)
	if _, err := sn.Prepare(ctx, "parentless", ""); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("parentless Prepare() error = %v, want invalid argument", err)
	}

	commitDelta(t, ctx, sn, "scratch-a", "")
	commitDelta(t, ctx, sn, "scratch-b", "scratch-a")
	if _, err := sn.Prepare(ctx, "container", "scratch-b"); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("Prepare() over committed scratch chain error = %v, want invalid argument", err)
	}
	if _, err := sn.Stat(ctx, "container"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(container) error = %v, want not found", err)
	}
}

func TestPrepareRollsBackOnMountFailure(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, notMounted)
	engine.mountChainFunc = func(context.Context, string, string, ...knarr.MountOption) error {
		return errors.New("mount failed")
	}
	if _, err := sn.Prepare(ctx, "container", leaf); err == nil {
		t.Fatal("Prepare() error = nil, want mount failure")
	}
	if _, err := sn.Stat(ctx, "container"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(container) error = %v, want not found", err)
	}
	engine.mountChainFunc = nil
	if _, err := sn.Prepare(ctx, "container", leaf); err != nil {
		t.Fatalf("Prepare() retry error = %v", err)
	}
}

func TestRemoveUnmountsBeforeDeletingChain(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, alwaysMounted)
	var target string
	engine.unmountFunc = func(_ context.Context, fsDir string) error {
		target = fsDir
		if _, err := os.Stat(filepath.Dir(fsDir)); err != nil {
			return fmt.Errorf("snapshot directory moved before unmount: %w", err)
		}
		return nil
	}
	id, _, err := sn.snapshotInfo(ctx, leaf)
	if err != nil {
		t.Fatalf("snapshotInfo(%s) error = %v", leaf, err)
	}
	if err := os.MkdirAll(filepath.Join(sn.getSnapshotDir(id), "fs"), 0o755); err != nil {
		t.Fatalf("create mount target: %v", err)
	}

	if err := sn.Remove(ctx, leaf); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if filepath.Base(target) != "fs" {
		t.Fatalf("Unmount target = %q, want .../fs", target)
	}
}

func TestRemoveAbortsWhenUnmountFails(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, alwaysMounted)
	engine.unmountFunc = func(context.Context, string) error { return errors.New("device busy") }

	if err := sn.Remove(ctx, leaf); err == nil {
		t.Fatal("Remove() error = nil, want unmount failure")
	}
	if _, err := sn.Stat(ctx, leaf); err != nil {
		t.Fatalf("Stat() after failed Remove = %v, want record intact", err)
	}
}

func TestRemoveWithChildDoesNotUnmount(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, alwaysMounted)
	var unmounts int
	engine.unmountFunc = func(context.Context, string) error { unmounts++; return nil }
	if _, err := sn.Prepare(ctx, "container", leaf); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Remove(ctx, leaf); err == nil {
		t.Fatal("Remove() error = nil, want child error")
	}
	if unmounts != 0 {
		t.Fatalf("Unmount calls = %d, want 0", unmounts)
	}
}

type fakeEngine struct {
	infos          map[string]*knarr.ImageInfo
	inspectFunc    func(context.Context, string) (*knarr.ImageInfo, error)
	mountChainFunc func(context.Context, string, string, ...knarr.MountOption) error
	unmountFunc    func(context.Context, string) error
	objectsDir     string
}

var _ engineAPI = (*fakeEngine)(nil)

func (e *fakeEngine) Inspect(ctx context.Context, ref string) (*knarr.ImageInfo, error) {
	if e.inspectFunc != nil {
		return e.inspectFunc(ctx, ref)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, ok := e.infos[ref]
	if !ok {
		return nil, fmt.Errorf("inspect %s: %w", ref, knarr.ErrNotFound)
	}
	return info, nil
}

func (e *fakeEngine) MountChain(ctx context.Context, chainID, target string, opts ...knarr.MountOption) error {
	if e.mountChainFunc != nil {
		return e.mountChainFunc(ctx, chainID, target, opts...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, info := range e.infos {
		if n := len(info.Layers); n > 0 && info.Layers[n-1].ChainID.String() == chainID {
			return nil
		}
	}
	return fmt.Errorf("mount chain %s: %w", chainID, knarr.ErrNotFound)
}

func (e *fakeEngine) Unmount(ctx context.Context, target string) error {
	if e.unmountFunc != nil {
		return e.unmountFunc(ctx, target)
	}
	return nil
}

func (e *fakeEngine) ObjectsDir() (string, error) {
	return e.objectsDir, nil
}

func fakeEngineForImages(infos ...*knarr.ImageInfo) *fakeEngine {
	engine := &fakeEngine{
		infos:      make(map[string]*knarr.ImageInfo, len(infos)),
		objectsDir: "/knarr/objects",
	}
	for _, info := range infos {
		addFakeImage(engine, info)
	}
	return engine
}

func addFakeImage(engine *fakeEngine, info *knarr.ImageInfo) {
	engine.infos[info.Root.String()] = info
}

type mountCall struct {
	chainID string
	target  string
	opts    knarr.MountOptions
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

func newBackedSnapshotter(t *testing.T, mounted func(string) (bool, error)) (*snapshotter, *fakeEngine, string) {
	t.Helper()
	ctx := context.Background()
	chain1 := digest.FromString("chain-1")
	chain2 := digest.FromString("chain-2")
	engine := fakeEngineForImages(testImageInfo("image-root", chain1, chain2))
	sn := newSnapshotterForTest(t, engine, mounted)
	seedChainRecord(t, ctx, sn, chain1.String(), "")
	seedChainRecord(t, ctx, sn, chain2.String(), chain1.String())
	return sn, engine, chain2.String()
}

func seedChainRecord(t *testing.T, ctx context.Context, sn *snapshotter, name, parent string) {
	t.Helper()
	if _, err := sn.Prepare(ctx, "extract-"+name, parent, snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: name,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("seed chain %s error = %v, want already exists", name, err)
	}
}

func commitDelta(t *testing.T, ctx context.Context, sn *snapshotter, name, parent string) {
	t.Helper()
	err := sn.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		delta, err := storage.CreateSnapshot(ctx, snapshots.KindActive, "active-"+name, parent)
		if err != nil {
			return err
		}
		if err := provisionOverlayDirs(sn.getSnapshotDir(delta.ID), snapshots.KindActive); err != nil {
			return err
		}
		_, err = storage.CommitActive(ctx, "active-"+name, name, snapshots.Usage{})
		return err
	})
	if err != nil {
		t.Fatalf("commit delta %s: %v", name, err)
	}
}

func recordMounts(engine *fakeEngine, calls *[]mountCall) {
	engine.mountChainFunc = func(_ context.Context, chainID, target string, opts ...knarr.MountOption) error {
		var mountOpts knarr.MountOptions
		for _, opt := range opts {
			opt(&mountOpts)
		}
		*calls = append(*calls, mountCall{chainID: chainID, target: target, opts: mountOpts})
		return nil
	}
}

// snapshotInfo returns the store id and info of the snapshot identified by key.
func (s *snapshotter) snapshotInfo(ctx context.Context, key string) (string, snapshots.Info, error) {
	var (
		id   string
		info snapshots.Info
	)
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var err error
		id, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return "", snapshots.Info{}, err
	}
	return id, info, nil
}

func notMounted(string) (bool, error)    { return false, nil }
func alwaysMounted(string) (bool, error) { return true, nil }

func testImageInfo(root string, chains ...digest.Digest) *knarr.ImageInfo {
	layers := make([]knarr.LayerInfo, len(chains))
	for i, chainID := range chains {
		layers[i].ChainID = chainID
	}
	return &knarr.ImageInfo{Root: digest.FromString(root), Layers: layers}
}

// optionValue extracts a key=value mount option, failing the test when absent.
func optionValue(t *testing.T, m mount.Mount, key string) string {
	t.Helper()
	for _, o := range m.Options {
		if v, ok := strings.CutPrefix(o, key+"="); ok {
			return v
		}
	}
	t.Fatalf("mount options %v carry no %s", m.Options, key)
	return ""
}
