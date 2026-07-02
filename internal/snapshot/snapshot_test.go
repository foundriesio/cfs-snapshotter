// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
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
	if _, err := NewSnapshotter(context.Background(), "", t.TempDir(), t.TempDir()); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("NewSnapshotter(%q) error = %v, want invalid argument", "", err)
	}
}

func TestNewSnapshotterAcceptsValidRoot(t *testing.T) {
	sn, err := NewSnapshotter(context.Background(), t.TempDir(), t.TempDir(), t.TempDir())
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
	return newTestSnapshotterWithEngine(t, defaultTestEngine())
}

// defaultTestEngine covers the single-layer image imageChain commits for
// seed "img".
func defaultTestEngine() *fakeEngine {
	return fakeEngineForImages(testImageInfo(digest.FromString("img-root"), digest.FromString("img")))
}

func newTestSnapshotterWithEngine(t *testing.T, engine engineAPI) snapshots.Snapshotter {
	t.Helper()
	sn, err := newSnapshotter(context.Background(), t.TempDir(), t.TempDir(), engine)
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

// imageChain commits an intermediate virtual snapshot the way a pull's unpack
// handshake would, for tests to layer snapshots over: the snapshotter refuses
// snapshots with no image in their ancestry. The committed name is the
// chainID of a single layer whose diffID derives from seed, so it resolves in
// an engine covering that layer — newTestSnapshotter's covers seed "img".
func imageChain(t *testing.T, ctx context.Context, sn snapshots.Snapshotter, seed string) string {
	t.Helper()
	name := chainIDs(digest.FromString(seed))[0].String()
	if _, err := sn.Prepare(ctx, "extract-"+name, "", snapshots.WithLabels(map[string]string{
		"containerd.io/snapshot.ref": name,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("image chain handshake error = %v, want already exists", err)
	}
	return name
}

// TestLifecycle covers the active → committed transition and that Walk lists the
// committed snapshots.
func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotter(t)
	img := imageChain(t, ctx, sn, "img")

	if _, err := sn.Prepare(ctx, "active", img); err != nil {
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
	want := map[string]bool{img: true, "committed": true}
	if len(walked) != len(want) {
		t.Fatalf("Walk() listed %v, want the chain and the committed snapshot", walked)
	}
	for _, name := range walked {
		if !want[name] {
			t.Fatalf("Walk() listed unexpected %q", name)
		}
	}

	viewMounts, err := sn.View(ctx, "view", "committed")
	if err != nil {
		t.Fatalf("View() error = %v", err)
	}
	if len(viewMounts) != 1 {
		t.Fatalf("View() returned %d mounts, want 1: %v", len(viewMounts), viewMounts)
	}
	if got := viewMounts[0].Options[0]; got != "ro" {
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
	img := imageChain(t, ctx, sn, "img")

	if _, err := sn.Prepare(ctx, "base", img); err != nil {
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
		intermediateSnapshotLabel: target1,
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
	if got := info.Labels[intermediateSnapshotLabel]; got != target1 {
		t.Fatalf("Stat(target1).Labels[%q] = %q, want %q", intermediateSnapshotLabel, got, target1)
	}

	if _, err := sn.Prepare(ctx, "extract-2", target1, snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: target2,
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
		intermediateSnapshotLabel: target1,
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
	if mounts[0].Type != "overlay" {
		t.Fatalf("Prepare(active) mount type = %q, want %q", mounts[0].Type, "overlay")
	}
}

func TestPrepareTargetMissingChainFailsClosed(t *testing.T) {
	ctx := context.Background()
	sn := newTestSnapshotterWithEngine(t, fakeEngineForImages())
	target := digest.FromString("missing-target").String()

	mounts, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: target,
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

	img := imageChain(t, ctx, sn, "img")
	const labelKey = "containerd.io/snapshot/cfs.test"
	opt := snapshots.WithLabels(map[string]string{labelKey: "applied"})
	if _, err := sn.Prepare(ctx, "labeled", img, opt); err != nil {
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
	img := imageChain(t, ctx, sn, "img")

	if _, err := sn.Prepare(ctx, "k", img); err != nil {
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
	img := imageChain(t, ctx, sn, "img")

	if _, err := sn.Prepare(ctx, "k", img); err != nil {
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
	sn, err := newSnapshotter(ctx, root, t.TempDir(), defaultTestEngine())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	img := imageChain(t, ctx, sn, "img")
	if _, err := sn.Prepare(ctx, "k", img); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Remove(ctx, "k"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := sn.Stat(ctx, "k"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat() after remove error = %v, want not found", err)
	}
	// The chain's materialized data plane belongs to the chain record and
	// survives its child; removing the chain drops it, children first as
	// the garbage collector would.
	if err := sn.Remove(ctx, img); err != nil {
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

	sn, err := newSnapshotter(ctx, root, t.TempDir(), defaultTestEngine())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	img := imageChain(t, ctx, sn, "img")
	if _, err := sn.Prepare(ctx, "active", img); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Commit(ctx, "committed", "active"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := sn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := newSnapshotter(ctx, root, t.TempDir(), defaultTestEngine())
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

// TestPrepareIntermediateSnapshotAnswersAlreadyExists exercises the unpack
// handshake: a Prepare carrying the snapshot.ref label must be answered with
// ErrAlreadyExists after committing an intermediate virtual snapshot under
// the requested name with the caller's labels, a repeat handshake must not
// leave an orphan active snapshot behind, and a Prepare without the label
// must keep taking the normal mount path.
func TestPrepareIntermediateSnapshotAnswersAlreadyExists(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sn, err := newSnapshotter(ctx, root, t.TempDir(), defaultTestEngine())
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	defer func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	name := chainIDs(digest.FromString("img"))[0].String()
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

	// Without the label the stub must stay out of the way.
	mounts, err := sn.Prepare(ctx, "container-1", name)
	if err != nil {
		t.Fatalf("plain Prepare error = %v", err)
	}
	if len(mounts) == 0 {
		t.Fatal("plain Prepare returned no mounts")
	}
}

// TestOverlayMountsComposeContainerFlow walks docker's container flow through
// the overlay data plane: the image chain snapshot stays virtual until a
// descendant needs it, materializes as a stand-in link at first use, writes
// into the init layer's upper resurface as a lower of the container prepared
// on the committed init, the walk stops at the nearest image so a longer
// image's containers never stack a shorter prefix image beneath it, and a
// scratch snapshot (no image in its ancestry) is refused. Misconfigured stand-in options
// are rejected at construction.
func TestOverlayMountsComposeContainerFlow(t *testing.T) {
	ctx := context.Background()
	standIn := t.TempDir()
	if err := os.WriteFile(standIn+"/hello", []byte("hi"), 0o755); err != nil {
		t.Fatalf("write stand-in: %v", err)
	}

	// Image A is a single layer; image B extends it with a second layer, so
	// their chains share A's prefix.
	diffA := digest.FromString("overlay-layer-a")
	diffB := digest.FromString("overlay-layer-b")
	chains := chainIDs(diffA, diffB)
	nameA, nameB := chains[0].String(), chains[1].String()
	engine := fakeEngineForImages(
		testImageInfo(digest.FromString("overlay-root-a"), diffA),
		testImageInfo(digest.FromString("overlay-root-b"), diffA, diffB),
	)

	if _, err := newSnapshotter(ctx, t.TempDir(), standIn+"/missing", engine); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("missing rootfs dir error = %v, want invalid argument", err)
	}
	if _, err := newSnapshotter(ctx, t.TempDir(), "", engine); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("empty rootfs error = %v, want invalid argument (the stand-in is required)", err)
	}

	root := t.TempDir()
	sn, err := newSnapshotter(ctx, root, standIn, engine)
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	defer func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	if _, err := sn.Prepare(ctx, "extract-1 "+nameA, "", snapshots.WithLabels(map[string]string{
		"containerd.io/snapshot.ref": nameA,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("handshake Prepare error = %v, want already exists", err)
	}

	// The handshake stays a pure metadata operation: no data plane exists
	// until a descendant snapshot needs the image.
	if entries, err := os.ReadDir(root + "/snapshots"); err != nil || len(entries) != 0 {
		t.Fatalf("snapshots dir = %v entries, %v; want none after the handshake", len(entries), err)
	}

	// The init layer: preparing it materializes the chain's data plane, and
	// its own upper starts empty over that base.
	imounts, err := sn.Prepare(ctx, "c1-init", nameA)
	if err != nil {
		t.Fatalf("init Prepare error = %v", err)
	}
	chainFS := optionValue(t, imounts[0], "lowerdir")
	if chainFS == standIn || !strings.HasSuffix(chainFS, "/fs") {
		t.Fatalf("init lowerdir = %q, want a chain snapshot fs entry", chainFS)
	}
	if target, err := os.Readlink(chainFS); err != nil || target != standIn {
		t.Fatalf("chain fs -> %q, %v; want a symlink to the stand-in", target, err)
	}
	initUpper := optionValue(t, imounts[0], "upperdir")
	if entries, err := os.ReadDir(initUpper); err != nil || len(entries) != 0 {
		t.Fatalf("init upper = %v entries, %v; want empty", len(entries), err)
	}

	// Docker writes its runtime files through the mounted overlay; they land
	// in the init snapshot's upper. Simulated at the directory level because
	// executing the overlay needs privileges.
	if err := os.MkdirAll(initUpper+"/etc", 0o755); err != nil {
		t.Fatalf("mkdir in init upper: %v", err)
	}
	if err := os.WriteFile(initUpper+"/etc/hostname", []byte("c1"), 0o644); err != nil {
		t.Fatalf("write into init upper: %v", err)
	}
	if err := sn.Commit(ctx, "c1-init-committed", "c1-init"); err != nil {
		t.Fatalf("Commit init error = %v", err)
	}

	// The container rootfs on the committed init: the init's upper stacks as
	// the nearest lower over the already-materialized chain.
	cmounts, err := sn.Prepare(ctx, "c1", "c1-init-committed")
	if err != nil {
		t.Fatalf("container Prepare error = %v", err)
	}
	if got, want := optionValue(t, cmounts[0], "lowerdir"), initUpper+":"+chainFS; got != want {
		t.Fatalf("container lowerdir = %q, want %q", got, want)
	}

	// A view of the committed init composes the same lowers read-only.
	vmounts, err := sn.View(ctx, "view-1", "c1-init-committed")
	if err != nil {
		t.Fatalf("View error = %v", err)
	}
	if vmounts[0].Options[0] != "ro" {
		t.Fatalf("view options = %v, want ro first", vmounts[0].Options)
	}
	if got, want := optionValue(t, vmounts[0], "lowerdir"), initUpper+":"+chainFS; got != want {
		t.Fatalf("view lowerdir = %q, want %q", got, want)
	}

	// A container prepared directly on the chain (ctr: no init layer)
	// composes the materialized chain alone — same treatment as the init.
	dmounts, err := sn.Prepare(ctx, "ctr-style-container", nameA)
	if err != nil {
		t.Fatalf("ctr-style Prepare error = %v", err)
	}
	if got := optionValue(t, dmounts[0], "lowerdir"); got != chainFS {
		t.Fatalf("ctr-style lowerdir = %q, want the chain fs %q", got, chainFS)
	}

	// A longer image extending image A's chain: its containers must stop at
	// their own chain and never stack A's materialized image beneath it —
	// a flattened image carries no whiteout markers, so a lower under it
	// would shine through deletions.
	if _, err := sn.Prepare(ctx, "extract-2 "+nameB, nameA, snapshots.WithLabels(map[string]string{
		"containerd.io/snapshot.ref": nameB,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("extending handshake Prepare error = %v, want already exists", err)
	}
	bmounts, err := sn.Prepare(ctx, "b-container", nameB)
	if err != nil {
		t.Fatalf("extended-image container Prepare error = %v", err)
	}
	blower := optionValue(t, bmounts[0], "lowerdir")
	if strings.Contains(blower, ":") || blower == chainFS || !strings.HasSuffix(blower, "/fs") {
		t.Fatalf("extended-image lowerdir = %q, want exactly one fs entry that is not image A's %q", blower, chainFS)
	}

	// No image in the ancestry: the stub serves only image-backed
	// snapshots, so a scratch snapshot is refused loudly at Prepare.
	if _, err := sn.Prepare(ctx, "scratch", ""); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("scratch Prepare error = %v, want invalid argument", err)
	}
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
