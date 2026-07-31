// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/foundriesio/knarr/pkg/knarr"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
	if _, err := NewSnapshotter(context.Background(), Config{KnarrRoot: t.TempDir()}); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("NewSnapshotter(%q) error = %v, want invalid argument", "", err)
	}
}

// TestNewSnapshotterServesUnstagedPulls covers that a snapshotter configured
// to stage unheld pulls comes up: staged layers are turned into knarr content
// when their chain completes.
func TestNewSnapshotterServesUnstagedPulls(t *testing.T) {
	sn, err := NewSnapshotter(context.Background(), Config{
		Root:               t.TempDir(),
		KnarrRoot:          t.TempDir(),
		AllowUnstagedPulls: true,
		ContainerdAddress:  "/run/containerd/containerd.sock",
	})
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	if err := sn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestNewSnapshotterAcceptsValidRoot(t *testing.T) {
	sn, err := NewSnapshotter(context.Background(), Config{Root: t.TempDir(), KnarrRoot: t.TempDir()})
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
			if _, err := newSnapshotter(ctx, Config{Root: t.TempDir() + sep + "root"}, engine); !errdefs.IsInvalidArgument(err) {
				t.Fatalf("newSnapshotter() root error = %v, want invalid argument", err)
			}
		})
		t.Run("objects "+sep, func(t *testing.T) {
			engine := &fakeEngine{objectsDir: t.TempDir() + sep + "objects"}
			if _, err := newSnapshotter(ctx, Config{Root: t.TempDir()}, engine); !errdefs.IsInvalidArgument(err) {
				t.Fatalf("newSnapshotter() objects error = %v, want invalid argument", err)
			}
		})
	}
}

func TestIsMountedTreatsMissingPathAsNotMounted(t *testing.T) {
	sn, err := newSnapshotter(context.Background(), Config{Root: t.TempDir()}, &fakeEngine{objectsDir: t.TempDir()})
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
	info := testImageInfo("root-"+name, digest.Digest(name))
	addFakeImage(sn.engine.(*fakeEngine), info)
	if _, err := sn.Prepare(ctx, "extract-"+name, "", snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: name,
		imageRefLabel:             info.Root.String(),
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
	sn, err := newSnapshotter(ctx, Config{Root: root}, engine)
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

	sn, err := newSnapshotter(ctx, Config{Root: root}, engine)
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

	reopened, err := newSnapshotter(ctx, Config{Root: root}, engine)
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
	sn, err := newSnapshotter(ctx, Config{Root: root}, engine)
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
	image := testImageInfo("handshake-root", digest.Digest(name))
	addFakeImage(engine, image)
	labels := map[string]string{
		intermediateSnapshotLabel: name,
		imageRefLabel:             image.Root.String(),
		"other":                   "kept",
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

func TestPrepareIntermediateSnapshotDeclinesUnheldImage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sn, err := newSnapshotter(ctx, Config{Root: root}, fakeEngineForImages())
	if err != nil {
		t.Fatalf("newSnapshotter() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	const name = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	if _, err := sn.Prepare(ctx, "extract-missing", "", snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: name,
		imageRefLabel:             digest.FromString("unheld-image").String(),
	})); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("handshake Prepare error = %v, want failed precondition", err)
	}
	if _, err := sn.Stat(ctx, name); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(%q) error = %v, want not found", name, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "snapshots"))
	if err != nil {
		t.Fatalf("read snapshots dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("snapshots dir has %d entries after declined handshake, want none", len(entries))
	}
}

func TestPrepareIntermediateSnapshotAcceptsNonLeafChain(t *testing.T) {
	ctx := context.Background()
	base := digest.FromString("base-chain")
	derived := testImageInfo("derived-image", base, digest.FromString("derived-chain"))
	sn := newSnapshotterForTest(t, fakeEngineForImages(derived), notMounted)

	if _, err := sn.Prepare(ctx, "extract-base", "", snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: base.String(),
		imageRefLabel:             derived.Root.String(),
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("non-leaf handshake Prepare error = %v, want already exists", err)
	}
	if info, err := sn.Stat(ctx, base.String()); err != nil {
		t.Fatalf("Stat(%q) error = %v", base, err)
	} else if info.Kind != snapshots.KindCommitted {
		t.Fatalf("Stat(%q).Kind = %v, want committed", base, info.Kind)
	}
}

func TestPrepareIntermediateSnapshotDeclinesMismatchedChain(t *testing.T) {
	ctx := context.Background()
	image := testImageInfo("held-image", digest.FromString("held-chain"))
	sn := newSnapshotterForTest(t, fakeEngineForImages(image), notMounted)
	requested := digest.FromString("other-chain").String()

	if _, err := sn.Prepare(ctx, "extract-mismatch", "", snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: requested,
		imageRefLabel:             image.Root.String(),
	})); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("mismatched handshake Prepare error = %v, want failed precondition", err)
	}
	if _, err := sn.Stat(ctx, requested); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(%q) error = %v, want not found", requested, err)
	}
}

func TestPrepareIntermediateSnapshotDeclinesMissingImageRef(t *testing.T) {
	ctx := context.Background()
	chain := digest.FromString("held-chain")
	sn := newSnapshotterForTest(t, fakeEngineForImages(testImageInfo("held-image", chain)), notMounted)

	if _, err := sn.Prepare(ctx, "extract-no-image-ref", "", snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: chain.String(),
	})); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("unstamped handshake Prepare error = %v, want failed precondition", err)
	}
	if _, err := sn.Stat(ctx, chain.String()); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(%q) error = %v, want not found", chain, err)
	}
}

func TestPrepareIntermediateSnapshotPropagatesInspectErrors(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	inspectErr := errors.New("inspect failed")
	tests := []struct {
		name       string
		ctx        context.Context
		inspectErr error
		wantErr    error
	}{
		{name: "engine error", ctx: context.Background(), inspectErr: inspectErr, wantErr: inspectErr},
		{name: "context cancellation", ctx: canceledCtx, wantErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := fakeEngineForImages()
			if tt.inspectErr != nil {
				engine.inspectFunc = func(context.Context, string) (*knarr.ImageInfo, error) {
					return nil, tt.inspectErr
				}
			}
			sn := newSnapshotterForTest(t, engine, notMounted)
			chain := digest.FromString("requested-chain").String()

			_, err := sn.Prepare(tt.ctx, "extract-error", "", snapshots.WithLabels(map[string]string{
				intermediateSnapshotLabel: chain,
				imageRefLabel:             digest.FromString("held-image").String(),
			}))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("handshake Prepare error = %v, want %v", err, tt.wantErr)
			}
			if _, err := sn.Stat(context.Background(), chain); !errdefs.IsNotFound(err) {
				t.Fatalf("Stat(%q) error = %v, want not found", chain, err)
			}
		})
	}
}

// TestPrepareStagesPulls covers the handshake in both work modes: the store
// holds no such image, or holds the image without the requested chain —
// declined when staging is off, answered with a writable snapshot when it is
// on — and an image the store does hold, which a staging snapshotter stages
// all the same instead of admitting, so the runtime fetches and keeps the
// layers itself.
func TestPrepareStagesPulls(t *testing.T) {
	ctx := context.Background()
	held := testImageInfo("held-image", digest.FromString("held-chain"))
	requested := digest.FromString("requested-chain").String()
	tests := []struct {
		name     string
		imageRef string
		images   []*knarr.ImageInfo
	}{
		{name: "image not held", imageRef: digest.FromString("unheld-image").String()},
		{name: "chain not a member", imageRef: held.Root.String(), images: []*knarr.ImageInfo{held}},
	}

	for _, tt := range tests {
		labels := map[string]string{
			intermediateSnapshotLabel: requested,
			imageRefLabel:             tt.imageRef,
		}
		t.Run(tt.name+" declined", func(t *testing.T) {
			sn := newSnapshotterForTest(t, fakeEngineForImages(tt.images...), notMounted)
			if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(labels)); !errdefs.IsFailedPrecondition(err) {
				t.Fatalf("handshake Prepare error = %v, want failed precondition", err)
			}
			if _, err := sn.Stat(ctx, "extract"); !errdefs.IsNotFound(err) {
				t.Fatalf("Stat(extract) error = %v, want not found", err)
			}
		})
		t.Run(tt.name+" staged", func(t *testing.T) {
			sn := newStagingSnapshotter(t, fakeEngineForImages(tt.images...))
			mounts, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(labels))
			if err != nil {
				t.Fatalf("handshake Prepare error = %v, want a staging snapshot", err)
			}
			id, info, err := sn.snapshotInfo(ctx, "extract")
			if err != nil {
				t.Fatalf("snapshotInfo(extract) error = %v", err)
			}
			if info.Kind != snapshots.KindActive || info.Labels[stagingLabel] == "" {
				t.Fatalf("staging snapshot info = %+v, want an active snapshot marked as staging", info)
			}
			upper := filepath.Join(sn.getSnapshotDir(id), "upper")
			if _, err := os.Stat(upper); err != nil {
				t.Fatalf("staging upper directory: %v", err)
			}
			// Nothing is staged below a first layer, so it is a plain bind.
			if len(mounts) != 1 || mounts[0].Type != "bind" || mounts[0].Source != upper ||
				!slices.Equal(mounts[0].Options, []string{"rw", "rbind"}) {
				t.Fatalf("mounts = %v, want a writable bind of %s", mounts, upper)
			}
		})
	}

	t.Run("held image staged all the same", func(t *testing.T) {
		sn := newStagingSnapshotter(t, fakeEngineForImages(held))
		labels := map[string]string{
			intermediateSnapshotLabel: held.Layers[len(held.Layers)-1].ChainID.String(),
			imageRefLabel:             held.Root.String(),
		}
		if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(labels)); err != nil {
			t.Fatalf("handshake Prepare error = %v, want a staging snapshot", err)
		}
		_, info, err := sn.snapshotInfo(ctx, "extract")
		if err != nil {
			t.Fatalf("snapshotInfo(extract) error = %v", err)
		}
		if info.Kind != snapshots.KindActive || info.Labels[stagingLabel] == "" {
			t.Fatalf("snapshot info = %+v, want an active snapshot marked as staging", info)
		}
	})
}

// TestCommitCarriesPrivateLabels covers that the snapshotter's own labels move
// from the active record onto the committed one, and that labels it does not
// own are left to the caller.
func TestCommitCarriesPrivateLabels(t *testing.T) {
	ctx := pullNamespace()
	sn := newStagingSnapshotter(t, fakeEngineForImages())
	root, chains := stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))
	labels := stagedLabels(digest.FromString("unheld-image").String(), root, chains[0])
	labels["other"] = "not ours"

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(labels)); err != nil {
		t.Fatalf("handshake Prepare error = %v", err)
	}
	if err := sn.Commit(ctx, chains[0].String(), "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	info, err := sn.Stat(ctx, chains[0].String())
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", chains[0], err)
	}
	if info.Labels[stagingLabel] == "" {
		t.Fatalf("committed snapshot labels = %v, want the staging marker carried over", info.Labels)
	}
	if _, ok := info.Labels["other"]; ok {
		t.Fatalf("committed snapshot labels = %v, want only the snapshotter's own carried over", info.Labels)
	}
}

// TestCommitBuildsStagedLeaf covers the conversion a cold pull ends with: the
// chain the record stands for is read from its snapshot.ref label rather than
// from the name containerd files it under, the image is built from the blobs
// the runtime fetched and pinned under this snapshotter, the record becomes an
// image chain member carrying the held digest, and the staged directories the
// chain no longer needs are discarded.
func TestCommitBuildsStagedLeaf(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))
	const imageRef = "registry.example/app:v1"

	dirs := make([]string, len(chains))
	parent := ""
	for i, chain := range chains {
		key := fmt.Sprintf("extract-%d", i)
		if _, err := sn.Prepare(ctx, key, parent, snapshots.WithLabels(stagedLabels(imageRef, root, chain))); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		id, _, err := sn.snapshotInfo(ctx, key)
		if err != nil {
			t.Fatalf("snapshotInfo(%s) error = %v", key, err)
		}
		dirs[i] = sn.getSnapshotDir(id)
		// containerd names a backend record for its own bookkeeping, never by
		// the chain the record stands for.
		parent = fmt.Sprintf("moby/%d/%s", i, chain)
		if err := sn.Commit(ctx, parent, key); err != nil {
			t.Fatalf("Commit(%s) error = %v", parent, err)
		}
	}

	if len(engine.pulls) != 1 {
		t.Fatalf("engine Pull calls = %+v, want only the leaf", engine.pulls)
	}
	pull := engine.pulls[0]
	if pull.ref != imageRef || pull.opts.ResolvedRoot != root || pull.opts.BlobSource != knarr.BlobSource(sn.blobs) {
		t.Fatalf("Pull(%q, %+v), want %q rooted at %s from the content store", pull.ref, pull.opts, imageRef, root)
	}
	want := []holdCall{{ref: root.String(), holder: cfsHolder}}
	if !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}

	leaf, err := sn.Stat(ctx, parent)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", parent, err)
	}
	if leaf.Labels[chainMemberLabel] == "" || leaf.Labels[rootDigestLabel] != root.String() {
		t.Fatalf("leaf labels = %v, want an image chain member holding %s", leaf.Labels, root)
	}
	if leaf.Labels[stagingLabel] != "" {
		t.Fatalf("leaf labels = %v, want the staging marker gone", leaf.Labels)
	}
	for i, dir := range dirs {
		for _, sub := range []string{"upper", "work"} {
			if _, err := os.Stat(filepath.Join(dir, sub)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staged %s of chain %d error = %v, want discarded", sub, i, err)
			}
		}
	}
}

// TestCommitLeavesNonLeafStagedChainAlone covers that only the chain's last
// layer builds an image: an earlier one is filed as it stands, still staging,
// with its directories intact for the layers stacking on top of it.
func TestCommitLeavesNonLeafStagedChainAlone(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	id, _, err := sn.snapshotInfo(ctx, "extract")
	if err != nil {
		t.Fatalf("snapshotInfo(extract) error = %v", err)
	}
	if err := sn.Commit(ctx, chains[0].String(), "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	if len(engine.pulls) != 0 || len(engine.holds) != 0 {
		t.Fatalf("engine pulls = %+v, holds = %+v; want neither for a non-leaf chain", engine.pulls, engine.holds)
	}
	info, err := sn.Stat(ctx, chains[0].String())
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Labels[stagingLabel] == "" || info.Labels[chainMemberLabel] != "" {
		t.Fatalf("committed labels = %v, want a staging record", info.Labels)
	}
	if _, err := os.Stat(filepath.Join(sn.getSnapshotDir(id), "upper")); err != nil {
		t.Fatalf("staged upper: %v, want it kept for the layers above", err)
	}
}

// TestCommitDeclinesUnanchoredStagedChain covers the labels a conversion is
// anchored on: without the manifest digest there is nothing to build against,
// and without a namespace there is no content store to read.
func TestCommitDeclinesUnanchoredStagedChain(t *testing.T) {
	root, chains := digest.FromString("root"), []digest.Digest{digest.FromString("chain")}
	tests := []struct {
		name    string
		ctx     context.Context
		labels  map[string]string
		wantErr func(error) bool
	}{
		{
			name: "no manifest digest",
			ctx:  pullNamespace(),
			labels: map[string]string{
				intermediateSnapshotLabel: chains[0].String(),
				imageRefLabel:             "app:v1",
			},
			wantErr: errdefs.IsFailedPrecondition,
		},
		{
			name:    "no namespace",
			ctx:     context.Background(),
			labels:  stagedLabels("app:v1", root, chains[0]),
			wantErr: errdefs.IsFailedPrecondition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := fakeEngineForImages()
			sn := newStagingSnapshotter(t, engine)
			stageImage(t, sn, digest.FromString("layer-1"))

			if _, err := sn.Prepare(tt.ctx, "extract", "", snapshots.WithLabels(tt.labels)); err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			err := sn.Commit(tt.ctx, "moby/1/chain", "extract")
			if err == nil || !tt.wantErr(err) {
				t.Fatalf("Commit() error = %v, want a declined conversion", err)
			}
			if len(engine.pulls) != 0 {
				t.Fatalf("engine pulls = %+v, want none", engine.pulls)
			}
			if _, err := sn.Stat(context.Background(), "moby/1/chain"); !errdefs.IsNotFound(err) {
				t.Fatalf("Stat() after declined commit = %v, want not found", err)
			}
		})
	}
}

// TestCommitPropagatesBuildFailure covers that a conversion that cannot
// complete fails the commit rather than filing a record for content the store
// does not have.
func TestCommitPropagatesBuildFailure(t *testing.T) {
	ctx := pullNamespace()
	buildErr := errors.New("build failed")
	engine := fakeEngineForImages()
	engine.pullFunc = func(context.Context, string, ...knarr.PullOption) (*knarr.Image, error) {
		return nil, buildErr
	}
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"))

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	name := "moby/1/" + chains[0].String()
	if err := sn.Commit(ctx, name, "extract"); !errors.Is(err, buildErr) {
		t.Fatalf("Commit() error = %v, want %v", err, buildErr)
	}
	if len(engine.holds) != 0 {
		t.Fatalf("engine holds = %+v, want none for a failed build", engine.holds)
	}
	if _, err := sn.Stat(ctx, name); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat() after failed commit = %v, want not found", err)
	}
}

// TestCommitFailsClosedWhenHoldFails covers a built image that cannot be
// pinned: without the hold nothing keeps a knarr remove from taking the
// content away, so the commit must fail rather than file the record.
func TestCommitFailsClosedWhenHoldFails(t *testing.T) {
	ctx := pullNamespace()
	holdErr := errors.New("hold failed")
	engine := fakeEngineForImages()
	engine.holdErr = holdErr
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"))

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	name := "moby/1/" + chains[0].String()
	if err := sn.Commit(ctx, name, "extract"); !errors.Is(err, holdErr) {
		t.Fatalf("Commit() error = %v, want %v", err, holdErr)
	}
	if len(engine.pulls) != 1 {
		t.Fatalf("engine Pull calls = %d, want the build to have run", len(engine.pulls))
	}
	if _, err := sn.Stat(ctx, name); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat() after failed commit = %v, want not found", err)
	}
}

// TestCommitConvergesOnDuplicateLeaves covers two pulls of the same image
// racing to commit the chain: both build it, which the content-addressed store
// deduplicates, and only one record survives.
func TestCommitConvergesOnDuplicateLeaves(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"))
	name := "moby/1/" + chains[0].String()

	for _, key := range []string{"extract-a", "extract-b"} {
		if _, err := sn.Prepare(ctx, key, "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
	}
	errs := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, key := range []string{"extract-a", "extract-b"} {
		go func() {
			start.Wait()
			errs <- sn.Commit(ctx, name, key)
		}()
	}
	start.Done()

	var failed int
	for range 2 {
		if err := <-errs; err != nil {
			if !errdefs.IsAlreadyExists(err) {
				t.Fatalf("Commit() error = %v, want already exists", err)
			}
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("%d of 2 concurrent commits failed, want exactly 1", failed)
	}
	if len(engine.pulls) != 2 {
		t.Fatalf("engine Pull calls = %d, want both commits to build", len(engine.pulls))
	}
	info, err := sn.Stat(ctx, name)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", name, err)
	}
	if info.Labels[rootDigestLabel] != root.String() {
		t.Fatalf("committed labels = %v, want the built image's digest", info.Labels)
	}
}

// TestCommitKeepsStagedAncestorWithOutsideChild covers the sweep stopping at a
// record another pull still stages on top of: its upper is a lower of that
// pull's mounts, and discarding it would pull the ground out from under a
// layer still being applied.
func TestCommitKeepsStagedAncestorWithOutsideChild(t *testing.T) {
	ctx := pullNamespace()
	sn := newStagingSnapshotter(t, fakeEngineForImages())
	root, chains := stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))

	if _, err := sn.Prepare(ctx, "extract-base", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare(extract-base) error = %v", err)
	}
	baseID, _, err := sn.snapshotInfo(ctx, "extract-base")
	if err != nil {
		t.Fatalf("snapshotInfo(extract-base) error = %v", err)
	}
	base := "moby/1/" + chains[0].String()
	if err := sn.Commit(ctx, base, "extract-base"); err != nil {
		t.Fatalf("Commit(base) error = %v", err)
	}
	// A second pull shares the prefix and is still applying its own layer on
	// top of the base while this pull completes.
	if _, err := sn.Prepare(ctx, "extract-other", base, snapshots.WithLabels(stagedLabels("other:v1", root, digest.FromString("other-chain")))); err != nil {
		t.Fatalf("Prepare(extract-other) error = %v", err)
	}
	if _, err := sn.Prepare(ctx, "extract-leaf", base, snapshots.WithLabels(stagedLabels("app:v1", root, chains[1]))); err != nil {
		t.Fatalf("Prepare(extract-leaf) error = %v", err)
	}
	leafID, _, err := sn.snapshotInfo(ctx, "extract-leaf")
	if err != nil {
		t.Fatalf("snapshotInfo(extract-leaf) error = %v", err)
	}
	if err := sn.Commit(ctx, "moby/2/"+chains[1].String(), "extract-leaf"); err != nil {
		t.Fatalf("Commit(leaf) error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(sn.getSnapshotDir(baseID), "upper")); err != nil {
		t.Fatalf("shared staged upper: %v, want it kept for the other pull", err)
	}
	if _, err := os.Stat(filepath.Join(sn.getSnapshotDir(leafID), "upper")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("converted leaf upper error = %v, want discarded", err)
	}
}

// TestRemoveReleasesByRecordedDigest covers that the hold a record took is
// released against the digest the record itself carries, so a reference that
// moved on to another image in the meantime cannot make the release land on
// the wrong one.
func TestRemoveReleasesByRecordedDigest(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"))
	name := "moby/1/" + chains[0].String()

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:latest", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Commit(ctx, name, "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	// The tag now points at another image; the release must not follow it.
	stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))

	if err := sn.Remove(ctx, name); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	want := []holdCall{{ref: root.String(), holder: cfsHolder}}
	if !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls = %+v, want %+v", engine.releases, want)
	}
}

// TestRemoveToleratesReleaseFailure covers removing a record whose image hold
// cannot be released: the record is already gone and the caller has nothing
// to retry, so the removal reports success and the hold lingers for a later
// release to clear.
func TestRemoveToleratesReleaseFailure(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	engine.releaseErr = errors.New("release failed")
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"))
	name := "moby/1/" + chains[0].String()

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:latest", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := sn.Commit(ctx, name, "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := sn.Remove(ctx, name); err != nil {
		t.Fatalf("Remove() error = %v, want success despite the failed release", err)
	}
	want := []holdCall{{ref: root.String(), holder: cfsHolder}}
	if !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls = %+v, want %+v", engine.releases, want)
	}
	if _, err := sn.Stat(ctx, name); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat() after remove = %v, want not found", err)
	}
}

// TestPrepareHoldsAdmittedImage covers that admitting a pull of an image the
// store already holds pins it too, and only once: the handshake for the last
// chain is the one that stands for the whole image, and the earlier ones are
// steps towards it.
func TestPrepareHoldsAdmittedImage(t *testing.T) {
	ctx := context.Background()
	base := digest.FromString("base-chain")
	leaf := digest.FromString("leaf-chain")
	image := testImageInfo("image", base, leaf)
	engine := fakeEngineForImages(image)
	sn := newSnapshotterForTest(t, engine, notMounted)

	seedChainRecord(t, ctx, sn, base.String(), "", image.Root.String())
	if len(engine.holds) != 0 {
		t.Fatalf("engine holds = %+v after a non-leaf handshake, want none", engine.holds)
	}
	if info, err := sn.Stat(ctx, base.String()); err != nil {
		t.Fatalf("Stat(%q) error = %v", base, err)
	} else if info.Labels[rootDigestLabel] != "" {
		t.Fatalf("non-leaf labels = %v, want no held digest", info.Labels)
	}

	seedChainRecord(t, ctx, sn, leaf.String(), base.String(), image.Root.String())
	want := []holdCall{{ref: image.Root.String(), holder: cfsHolder}}
	if !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}
	info, err := sn.Stat(ctx, leaf.String())
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", leaf, err)
	}
	if info.Labels[rootDigestLabel] != image.Root.String() {
		t.Fatalf("leaf labels = %v, want the held digest %s", info.Labels, image.Root)
	}
}

// TestPrepareAdmissionFailsClosedWhenHoldFails covers admitting a leaf whose
// image cannot be pinned: the handshake must fail rather than record a chain
// a knarr remove could then take the content away from.
func TestPrepareAdmissionFailsClosedWhenHoldFails(t *testing.T) {
	ctx := context.Background()
	holdErr := errors.New("hold failed")
	chain := digest.FromString("leaf-chain")
	image := testImageInfo("image", chain)
	engine := fakeEngineForImages(image)
	engine.holdErr = holdErr
	sn := newSnapshotterForTest(t, engine, notMounted)

	labels := map[string]string{
		intermediateSnapshotLabel: chain.String(),
		imageRefLabel:             image.Root.String(),
	}
	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(labels)); !errors.Is(err, holdErr) {
		t.Fatalf("Prepare() error = %v, want %v", err, holdErr)
	}
	if _, err := sn.Stat(ctx, chain.String()); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(%q) error = %v, want not found", chain, err)
	}
}

// TestUsageIsComputedForCommittedRecords covers the two committed records
// whose recorded usage cannot be trusted: one standing for an image costs that
// image's composefs blob, and one whose staged directories were discarded
// costs nothing.
func TestUsageIsComputedForCommittedRecords(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))
	engine.infos[root.String()] = &knarr.ImageInfo{
		Root:      root,
		Composefs: ocispec.Descriptor{Size: 4096},
	}

	parent := ""
	names := make([]string, len(chains))
	for i, chain := range chains {
		key := fmt.Sprintf("extract-%d", i)
		if _, err := sn.Prepare(ctx, key, parent, snapshots.WithLabels(stagedLabels("app:v1", root, chain))); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		names[i] = fmt.Sprintf("moby/%d/%s", i, chain)
		if err := sn.Commit(ctx, names[i], key); err != nil {
			t.Fatalf("Commit(%s) error = %v", names[i], err)
		}
		parent = names[i]
	}

	if usage, err := sn.Usage(ctx, names[1]); err != nil {
		t.Fatalf("Usage(leaf) error = %v", err)
	} else if usage.Size != 4096 {
		t.Fatalf("Usage(leaf) = %+v, want the composefs size", usage)
	}
	if usage, err := sn.Usage(ctx, names[0]); err != nil {
		t.Fatalf("Usage(staged) error = %v", err)
	} else if usage != (snapshots.Usage{}) {
		t.Fatalf("Usage(staged) = %+v, want nothing after the sweep", usage)
	}
}

// TestCloseIsIdempotent covers tearing the snapshotter down twice, which the
// containerd connection would otherwise report as already closed.
func TestCloseIsIdempotent(t *testing.T) {
	sn, err := newSnapshotter(context.Background(), Config{
		Root:              t.TempDir(),
		ContainerdAddress: filepath.Join(t.TempDir(), "containerd.sock"),
	}, fakeEngineForImages())
	if err != nil {
		t.Fatalf("newSnapshotter() error = %v", err)
	}
	if _, err := sn.blobs.contentStore(); err != nil {
		t.Fatalf("contentStore() error = %v", err)
	}
	for i := range 2 {
		if err := sn.Close(); err != nil {
			t.Fatalf("Close() call %d error = %v", i+1, err)
		}
	}
}

// TestUpdateKeepsPrivateLabels covers that a whole-map label update, which
// reaches the backend with everything outside the containerd.io namespace
// already stripped, does not take the snapshotter's own markers off the
// record.
func TestUpdateKeepsPrivateLabels(t *testing.T) {
	ctx := context.Background()
	sn := newSnapshotterForTest(t, fakeEngineForImages(), notMounted)
	chain := digest.FromString("updated-chain").String()
	commitChainRecord(t, ctx, sn, chain, chain)

	info, err := sn.Update(ctx, snapshots.Info{
		Name: chain,
		Labels: map[string]string{
			intermediateSnapshotLabel: chain,
			"containerd.io/gc.root":   "keep",
		},
	}, "labels")
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if info.Labels[chainMemberLabel] == "" {
		t.Fatalf("updated labels = %v, want the chain-member marker kept", info.Labels)
	}
	if info.Labels["containerd.io/gc.root"] != "keep" {
		t.Fatalf("updated labels = %v, want the caller's label applied", info.Labels)
	}
	stat, err := sn.Stat(ctx, chain)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", chain, err)
	}
	if stat.Labels[chainMemberLabel] == "" {
		t.Fatalf("stored labels = %v, want the chain-member marker kept", stat.Labels)
	}
}

// TestStagingMountsComposeStagedUppers covers a staging snapshot over staged
// ancestors: a plain overlay of their uppers, nearest parent first, with no
// image, no knarr objects, and no fs-verity beneath it.
func TestStagingMountsComposeStagedUppers(t *testing.T) {
	ctx := pullNamespace()
	sn := newStagingSnapshotter(t, fakeEngineForImages())
	root, chains := stageImage(t, sn,
		digest.FromString("layer-1"), digest.FromString("layer-2"),
		digest.FromString("layer-3"), digest.FromString("layer-4"))
	imageRef := digest.FromString("unheld-image").String()
	stage := func(key, parent string, chain digest.Digest) ([]mount.Mount, string) {
		t.Helper()
		mounts, err := sn.Prepare(ctx, key, parent, snapshots.WithLabels(stagedLabels(imageRef, root, chain)))
		if err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		id, _, err := sn.snapshotInfo(ctx, key)
		if err != nil {
			t.Fatalf("snapshotInfo(%s) error = %v", key, err)
		}
		return mounts, sn.getSnapshotDir(id)
	}

	_, first := stage("extract-1", "", chains[0])
	if err := sn.Commit(ctx, chains[0].String(), "extract-1"); err != nil {
		t.Fatalf("Commit(chain-1) error = %v", err)
	}
	_, second := stage("extract-2", chains[0].String(), chains[1])
	if err := sn.Commit(ctx, chains[1].String(), "extract-2"); err != nil {
		t.Fatalf("Commit(chain-2) error = %v", err)
	}
	mounts, third := stage("extract-3", chains[1].String(), chains[2])

	want := []string{
		"workdir=" + filepath.Join(third, "work"),
		"upperdir=" + filepath.Join(third, "upper"),
		"lowerdir=" + filepath.Join(second, "upper") + ":" + filepath.Join(first, "upper"),
	}
	if len(mounts) != 1 || mounts[0].Type != "overlay" || !slices.Equal(mounts[0].Options, want) {
		t.Fatalf("mounts = %v, want a plain overlay with options %v", mounts, want)
	}
}

// TestMountsClassifyChainMemberByLabel covers that a chain snapshot that has
// picked up staging directories is still where the mount's parent walk stops.
func TestMountsClassifyChainMemberByLabel(t *testing.T) {
	ctx := context.Background()
	sn, engine, leaf := newBackedSnapshotter(t, notMounted)
	id, _, err := sn.snapshotInfo(ctx, leaf)
	if err != nil {
		t.Fatalf("snapshotInfo(%s) error = %v", leaf, err)
	}
	if err := provisionOverlayDirs(sn.getSnapshotDir(id), snapshots.KindActive); err != nil {
		t.Fatalf("provision leftover directories: %v", err)
	}
	var calls []mountCall
	recordMounts(engine, &calls)

	mounts, err := sn.Prepare(ctx, "container", leaf)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(calls) != 1 || calls[0].chainID != leaf {
		t.Fatalf("MountChain calls = %+v, want the image chain %q", calls, leaf)
	}
	if got := optionValue(t, mounts[0], "lowerdir"); got != calls[0].target+"::"+engine.objectsDir {
		t.Fatalf("lowerdir = %q, want the image mount only", got)
	}
}

// TestMountsUseChainIDFromLabel covers that the image is mounted by the
// chainID its snapshot.ref label records, not by the name the caller filed the
// record under.
func TestMountsUseChainIDFromLabel(t *testing.T) {
	ctx := context.Background()
	chain := digest.FromString("labeled-chain").String()
	engine := fakeEngineForImages(testImageInfo("image", digest.Digest(chain)))
	sn := newSnapshotterForTest(t, engine, notMounted)
	const record = "moby/7/sha256:not-a-chain-id"
	commitChainRecord(t, ctx, sn, record, chain)
	var calls []mountCall
	recordMounts(engine, &calls)

	if _, err := sn.Prepare(ctx, "container", record); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(calls) != 1 || calls[0].chainID != chain {
		t.Fatalf("MountChain calls = %+v, want the label's chain %q", calls, chain)
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
	seedChainRecord(t, ctx, sn, chain.String(), "", derived.Root.String())

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
	imageA := testImageInfo("a", chainA)
	imageB := testImageInfo("b", chainB)
	engine := fakeEngineForImages(imageA, imageB)
	sn := newSnapshotterForTest(t, engine, notMounted)
	seedChainRecord(t, ctx, sn, chainA.String(), "", imageA.Root.String())
	seedChainRecord(t, ctx, sn, chainB.String(), chainA.String(), imageB.Root.String())
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
	pullFunc       func(context.Context, string, ...knarr.PullOption) (*knarr.Image, error)
	objectsDir     string
	holdErr        error
	releaseErr     error

	// mu guards the call records, which concurrent commits append to.
	mu       sync.Mutex
	pulls    []pullCall
	holds    []holdCall
	releases []holdCall
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

func (e *fakeEngine) Pull(ctx context.Context, ref string, opts ...knarr.PullOption) (*knarr.Image, error) {
	var pullOpts knarr.PullOptions
	for _, opt := range opts {
		opt(&pullOpts)
	}
	e.mu.Lock()
	e.pulls = append(e.pulls, pullCall{ref: ref, opts: pullOpts})
	e.mu.Unlock()
	if e.pullFunc != nil {
		return e.pullFunc(ctx, ref, opts...)
	}
	return &knarr.Image{Ref: ref, Digest: pullOpts.ResolvedRoot.String()}, nil
}

func (e *fakeEngine) Hold(_ context.Context, ref, holder string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.holds = append(e.holds, holdCall{ref: ref, holder: holder})
	return e.holdErr
}

func (e *fakeEngine) Release(_ context.Context, ref, holder string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.releases = append(e.releases, holdCall{ref: ref, holder: holder})
	return e.releaseErr
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

type pullCall struct {
	ref  string
	opts knarr.PullOptions
}

type holdCall struct {
	ref    string
	holder string
}

// fakeContent serves canned blobs the way containerd's content store does.
// Only the read a conversion makes is implemented; anything else panics on the
// nil embedded store.
type fakeContent struct {
	content.Store
	blobs map[digest.Digest][]byte
}

func (f fakeContent) ReaderAt(_ context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	blob, ok := f.blobs[desc.Digest]
	if !ok {
		return nil, fmt.Errorf("content %s: %w", desc.Digest, errdefs.ErrNotFound)
	}
	return fakeReaderAt{Reader: bytes.NewReader(blob)}, nil
}

type fakeReaderAt struct {
	*bytes.Reader
}

func (r fakeReaderAt) Size() int64 { return r.Reader.Size() }

func (fakeReaderAt) Close() error { return nil }

// stageImage puts the manifest and config of an image with the given layer
// diff IDs in the snapshotter's content store, and returns the manifest digest
// the runtime would label a pull with along with the chain ID of each layer.
func stageImage(t *testing.T, sn *snapshotter, diffIDs ...digest.Digest) (digest.Digest, []digest.Digest) {
	t.Helper()
	config, err := json.Marshal(ocispec.Image{
		RootFS: ocispec.RootFS{Type: "layers", DiffIDs: diffIDs},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configDigest := digest.FromBytes(config)
	manifest, err := json.Marshal(ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(config)),
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	root := digest.FromBytes(manifest)
	sn.blobs.store = fakeContent{blobs: map[digest.Digest][]byte{
		root:         manifest,
		configDigest: config,
	}}
	chains := make([]digest.Digest, len(diffIDs))
	copy(chains, diffIDs)
	identity.ChainIDs(chains)
	return root, chains
}

// stagedLabels are the labels the runtime attaches to a pull of an image the
// knarr store does not hold, which Prepare answers with a staging snapshot.
func stagedLabels(imageRef string, root, chainID digest.Digest) map[string]string {
	return map[string]string{
		intermediateSnapshotLabel: chainID.String(),
		imageRefLabel:             imageRef,
		manifestDigestLabel:       root.String(),
	}
}

// pullNamespace returns a context naming the namespace a docker pull is served
// in, which is where its blobs live in the content store.
func pullNamespace() context.Context {
	return namespaces.WithNamespace(context.Background(), "moby")
}

func newSnapshotterForTest(t *testing.T, engine *fakeEngine, mounted func(string) (bool, error)) *snapshotter {
	t.Helper()
	return newConfiguredSnapshotter(t, Config{Root: t.TempDir()}, engine, mounted)
}

// newStagingSnapshotter returns a snapshotter configured to stage pulls of
// images the knarr store does not hold.
func newStagingSnapshotter(t *testing.T, engine *fakeEngine) *snapshotter {
	t.Helper()
	return newConfiguredSnapshotter(t, Config{Root: t.TempDir(), AllowUnstagedPulls: true}, engine, notMounted)
}

func newConfiguredSnapshotter(t *testing.T, cfg Config, engine *fakeEngine, mounted func(string) (bool, error)) *snapshotter {
	t.Helper()
	sn, err := newSnapshotter(context.Background(), cfg, engine)
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
	image := testImageInfo("image-root", chain1, chain2)
	engine := fakeEngineForImages(image)
	sn := newSnapshotterForTest(t, engine, mounted)
	seedChainRecord(t, ctx, sn, chain1.String(), "", image.Root.String())
	seedChainRecord(t, ctx, sn, chain2.String(), chain1.String(), image.Root.String())
	return sn, engine, chain2.String()
}

func seedChainRecord(t *testing.T, ctx context.Context, sn *snapshotter, name, parent, imageRef string) {
	t.Helper()
	if _, err := sn.Prepare(ctx, "extract-"+name, parent, snapshots.WithLabels(map[string]string{
		intermediateSnapshotLabel: name,
		imageRefLabel:             imageRef,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("seed chain %s error = %v, want already exists", name, err)
	}
}

// commitChainRecord commits a metadata-only chain snapshot under name, the way
// containerd files a backend's records: the name is the caller's own
// bookkeeping and only the snapshot.ref label names the layer chain.
func commitChainRecord(t *testing.T, ctx context.Context, sn *snapshotter, name, chainID string) {
	t.Helper()
	err := sn.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		opt := snapshots.WithLabels(map[string]string{
			intermediateSnapshotLabel: chainID,
			chainMemberLabel:          "true",
		})
		if _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, "active-"+name, "", opt); err != nil {
			return err
		}
		_, err := storage.CommitActive(ctx, "active-"+name, name, snapshots.Usage{}, opt)
		return err
	})
	if err != nil {
		t.Fatalf("commit chain record %s: %v", name, err)
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
