// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	contentapi "github.com/containerd/containerd/api/services/content/v1"
	imagesapi "github.com/containerd/containerd/api/services/images/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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
	if _, err := NewSnapshotter(context.Background(), Config{OcicfsRoot: t.TempDir()}); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("NewSnapshotter(%q) error = %v, want invalid argument", "", err)
	}
}

// verityOcicfsRoot returns a fresh ocicfs root on the dev container's shared
// fs-verity filesystem, skipping where that filesystem is absent, so a build
// made in auto or required mode runs the store with fs-verity; t.TempDir may
// sit on a filesystem that cannot enable it, which a required build refuses.
func verityOcicfsRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/lib/cfs-shared-root", "ocicfs-test-*")
	if err != nil {
		t.Skipf("verity-capable filesystem unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})
	return dir
}

// TestNewSnapshotterAdmitsAny covers that a snapshotter configured
// to admit any image comes up: staged layers are turned into ocicfs content
// when their chain completes.
func TestNewSnapshotterAdmitsAny(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	sn, err := NewSnapshotter(context.Background(), Config{
		Root:              t.TempDir(),
		OcicfsRoot:        verityOcicfsRoot(t),
		AdmitAny:          true,
		ContainerdAddress: "/run/containerd/containerd.sock",
	})
	if err != nil {
		t.Fatalf("NewSnapshotter() error = %v", err)
	}
	if err := sn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestNewSnapshotterFailsFastWithoutStagedPrerequisites covers the fail-fast
// gate: an admit-any snapshotter refuses to come up when the host lacks what
// staging needs, instead of failing at the first pull.
func TestNewSnapshotterFailsFastWithoutStagedPrerequisites(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := NewSnapshotter(context.Background(), Config{
		Root:              t.TempDir(),
		OcicfsRoot:        t.TempDir(),
		AdmitAny:          true,
		ContainerdAddress: "/run/containerd/containerd.sock",
	})
	if err == nil || !strings.Contains(err.Error(), "mkcomposefs") {
		t.Fatalf("NewSnapshotter() error = %v, want a prerequisites failure naming mkcomposefs", err)
	}
	if !errors.Is(err, ErrPrerequisites) {
		t.Fatalf("NewSnapshotter() error = %v, want ErrPrerequisites so the daemon exits with the no-restart status", err)
	}
}

// TestNewSnapshotterAcceptsValidRoot covers that a preloaded snapshotter comes
// up on a store this build can run, and refuses one it cannot: a required
// build on a filesystem that cannot enable fs-verity. Which one t.TempDir is
// depends on the host and the build mode, so a throwaway engine on the same
// store decides what to expect.
func TestNewSnapshotterAcceptsValidRoot(t *testing.T) {
	ocicfsRoot := t.TempDir()
	probe, err := ocicfs.New(ocicfs.Config{Root: ocicfsRoot})
	if err != nil {
		t.Fatalf("ocicfs.New() error = %v", err)
	}
	state, err := probe.VerityState()
	if err != nil {
		t.Fatalf("VerityState() error = %v", err)
	}
	sn, err := NewSnapshotter(context.Background(), Config{Root: t.TempDir(), OcicfsRoot: ocicfsRoot})
	if state == "unsupported" {
		if !errors.Is(err, ErrPrerequisites) {
			t.Fatalf("NewSnapshotter() error = %v, want ErrPrerequisites: this build requires fs-verity and the store's filesystem cannot enable it", err)
		}
		return
	}
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
			if _, err := NewSnapshotter(ctx, Config{Root: t.TempDir() + sep + "root", OcicfsRoot: t.TempDir()}); !errdefs.IsInvalidArgument(err) {
				t.Fatalf("NewSnapshotter() root error = %v, want invalid argument", err)
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
// fake ocicfs store.
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
				engine.inspectFunc = func(context.Context, string) (*ocicfs.ImageInfo, error) {
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
// declined when staging is off, answered with a staging snapshot when it is
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
		images   []*ocicfs.ImageInfo
	}{
		{name: "image not held", imageRef: digest.FromString("unheld-image").String()},
		{name: "chain not a member", imageRef: held.Root.String(), images: []*ocicfs.ImageInfo{held}},
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
			if _, err := os.Stat(sn.getSnapshotDir(id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging snapshot directory error = %v, want none provisioned", err)
			}
			if !isStagingMount(mounts, "extract") {
				t.Fatalf("mounts = %v, want the staging placeholder naming the record", mounts)
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

// TestCommitCarriesChainLabel covers that the chain a staged record stands
// for survives a Commit that does not pass it again, the way a caller
// commits a record whose Prepare this snapshotter labeled: a leaf and a
// record below it both keep their snapshot.ref label.
func TestCommitCarriesChainLabel(t *testing.T) {
	ctx := pullNamespace()
	sn := newStagingSnapshotter(t, fakeEngineForImages())
	x, y := digest.FromString("layer-x"), digest.FromString("layer-y")
	root, chains := stageImage(t, sn, x, y)
	const ref = "registry.example/a:v1"

	parent := ""
	for i, chain := range chains {
		key := fmt.Sprintf("moby/%d/extract", i)
		if _, err := sn.Prepare(ctx, key, parent, snapshots.WithLabels(stagedLabels(ref, root, chain))); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if err := applyStaged(ctx, sn, key, []digest.Digest{x, y}[i]); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
		name := fmt.Sprintf("moby/%d/%s", i, chain)
		if err := sn.Commit(ctx, name, key); err != nil {
			t.Fatalf("Commit(%s) error = %v", name, err)
		}
		info, err := sn.Stat(ctx, name)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", name, err)
		}
		if info.Labels[intermediateSnapshotLabel] != chain.String() {
			t.Fatalf("committed record %s labels = %v, want %s = %s", name, info.Labels, intermediateSnapshotLabel, chain)
		}
		parent = name
	}
}

// TestApplyDrivesBuild covers the conversion a cold pull drives: the chain
// each record stands for is read from its snapshot.ref label rather than from
// the name containerd files it under, every Apply feeds its layer to one build
// of the image from the blobs the runtime fetched, the last one finishes the
// build, the leaf's Commit pins the image under this snapshotter and its
// record becomes an image chain member carrying the held digest, and no
// record ever owns a directory.
func TestApplyDrivesBuild(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2"), digest.FromString("layer-3")}
	root, chains := stageImage(t, sn, layers...)
	const imageRef = "registry.example/app:v1"

	parent := ""
	for i, chain := range chains {
		key := fmt.Sprintf("moby/%d/extract", i)
		mounts, err := sn.Prepare(ctx, key, parent, snapshots.WithLabels(stagedLabels(imageRef, root, chain)))
		if err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		applied, err := sn.Apply(ctx, ocispec.Descriptor{Digest: layerBlob(layers[i])}, mounts)
		if err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
		if applied.Digest != layers[i] || applied.MediaType != ocispec.MediaTypeImageLayer {
			t.Fatalf("Apply(%s) = %+v, want the layer's diff ID as an uncompressed layer", key, applied)
		}
		// containerd names a backend record for its own bookkeeping, never by
		// the chain the record stands for.
		parent = fmt.Sprintf("moby/%d/%s", i, chain)
		if err := sn.Commit(ctx, parent, key); err != nil {
			t.Fatalf("Commit(%s) error = %v", parent, err)
		}
	}

	if len(engine.builds) != 1 {
		t.Fatalf("engine NewBuild calls = %+v, want one for the whole pull", engine.builds)
	}
	build := engine.builds[0]
	if build.ref != imageRef || build.opts.ResolvedRoot != root || build.opts.BlobSource != ocicfs.BlobSource(sn.blobs) {
		t.Fatalf("NewBuild(%q, %+v), want %q rooted at %s from the content store", build.ref, build.opts, imageRef, root)
	}
	wantCalls := []string{"build0.ingest(0)", "build0.ingest(1)", "build0.finish"}
	if !slices.Equal(engine.calls, wantCalls) {
		t.Fatalf("engine calls = %v, want %v", engine.calls, wantCalls)
	}
	// The build holds the image when it finishes, under a holder named after
	// the staging snapshot that started it; no commit holds or releases.
	holder := buildHolder("moby", "moby/0/extract")
	want := []holdCall{{ref: root.String(), holder: holder}}
	if !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}
	if len(sn.builds) != 0 {
		t.Fatalf("open builds after the leaf = %d, want none", len(sn.builds))
	}

	leaf, err := sn.Stat(ctx, parent)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", parent, err)
	}
	if leaf.Labels[chainMemberLabel] == "" || leaf.Labels[rootDigestLabel] != root.String() || leaf.Labels[holderLabel] != holder {
		t.Fatalf("leaf labels = %v, want an image chain member holding %s under %s", leaf.Labels, root, holder)
	}
	if leaf.Labels[stagingLabel] != "" {
		t.Fatalf("leaf labels = %v, want the staging marker gone", leaf.Labels)
	}
	entries, err := os.ReadDir(filepath.Join(sn.root, "snapshots"))
	if err != nil {
		t.Fatalf("read snapshots dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("snapshots dir has %d entries after a staged pull, want none", len(entries))
	}
}

// TestApplyDeclinesForeignMounts covers the differ being tried first for a
// pull it has no part in: mounts of any other shape are declined as not
// implemented, so containerd's diff service falls through to the next
// differ, and no build is started.
func TestApplyDeclinesForeignMounts(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	tests := []struct {
		name   string
		mounts []mount.Mount
	}{
		{name: "overlay", mounts: []mount.Mount{{Type: "overlay", Source: "overlay", Options: []string{"lowerdir=/a:/b"}}}},
		{name: "bind", mounts: []mount.Mount{{Type: "bind", Source: "/upper", Options: []string{"rw", "rbind"}}}},
		{name: "none", mounts: nil},
		{name: "placeholder among others", mounts: append(stagingMount("extract"), mount.Mount{Type: "bind", Source: "/x"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sn.Apply(ctx, ocispec.Descriptor{Digest: digest.FromString("blob")}, tt.mounts)
			if !errdefs.IsNotImplemented(err) {
				t.Fatalf("Apply() error = %v, want not implemented", err)
			}
		})
	}
	if len(engine.builds) != 0 || len(engine.calls) != 0 {
		t.Fatalf("engine builds = %+v, calls = %v; want none", engine.builds, engine.calls)
	}
}

// TestApplyRequiresStagingRecord covers the placeholder's source naming
// something other than an active staging snapshot: a missing record, a
// container's own snapshot, or a record already committed.
func TestApplyRequiresStagingRecord(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))
	// A container's own snapshot: active, but not staging.
	err := sn.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, err := storage.CreateSnapshot(ctx, snapshots.KindActive, "container", "")
		return err
	})
	if err != nil {
		t.Fatalf("create container record: %v", err)
	}
	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare(extract) error = %v", err)
	}
	if err := sn.Commit(ctx, "committed", "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	for _, key := range []string{"missing", "container", "committed"} {
		t.Run(key, func(t *testing.T) {
			_, err := sn.Apply(ctx, ocispec.Descriptor{Digest: digest.FromString("blob")}, stagingMount(key))
			if err == nil || errdefs.IsNotImplemented(err) {
				t.Fatalf("Apply() error = %v, want the record refused", err)
			}
		})
	}
	if len(engine.builds) != 0 {
		t.Fatalf("engine builds = %+v, want none", engine.builds)
	}
}

// TestApplyDeclinesLabeledUnpackOutOfBand covers an unpack handshake whose
// key names the chain and whose labels name a pulled image, on a daemon that
// builds nothing from runtime content: the Apply is declined with a failed
// precondition before any of the runtime's content is read, and nothing is
// built.
func TestApplyDeclinesLabeledUnpackOutOfBand(t *testing.T) {
	ctx := pullNamespace()
	x := digest.FromString("layer-1")
	engine := fakeEngineForImages()
	sn := newSnapshotterForTest(t, engine, notMounted)
	root, chains := stageImage(t, sn, x)
	loadLayers(sn, x)
	key := loadKey(0, chains[0])
	labels := map[string]string{imageRefLabel: "app:v1", manifestDigestLabel: root.String()}
	if _, err := sn.Prepare(ctx, key, "", snapshots.WithLabels(labels)); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	// Any read of the runtime's content panics on the nil client.
	sn.blobs.client = struct{ contentapi.ContentClient }{}
	if err := applyStaged(ctx, sn, key, x); !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), "run ocicfs pull <image>") {
		t.Fatalf("Apply(%s) error = %v, want a failed precondition naming ocicfs pull", key, err)
	}
	if len(engine.builds) != 0 || imagesOf(sn).listCount() != 0 {
		t.Fatalf("engine builds = %+v, image listings = %d; want none", engine.builds, imagesOf(sn).listCount())
	}
}

// TestApplyRefusesMismatchedLayer covers the binding of the descriptor to the
// manifest: the blob containerd hands the differ must be the record's layer,
// with the media type and size the manifest declares when they are given, so
// a mount left over from another pull can never label the wrong blob.
func TestApplyRefusesMismatchedLayer(t *testing.T) {
	ctx := pullNamespace()
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	tests := []struct {
		name string
		desc ocispec.Descriptor
	}{
		{name: "other layer", desc: ocispec.Descriptor{Digest: layerBlob(layers[1])}},
		{name: "unknown blob", desc: ocispec.Descriptor{Digest: digest.FromString("elsewhere")}},
		{name: "media type", desc: ocispec.Descriptor{Digest: layerBlob(layers[0]), MediaType: ocispec.MediaTypeImageLayerZstd}},
		{name: "size", desc: ocispec.Descriptor{Digest: layerBlob(layers[0]), Size: 7}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := fakeEngineForImages()
			sn := newStagingSnapshotter(t, engine)
			root, chains := stageImage(t, sn, layers...)
			if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if _, err := sn.Apply(ctx, tt.desc, stagingMount("extract")); !errdefs.IsFailedPrecondition(err) {
				t.Fatalf("Apply() error = %v, want failed precondition", err)
			}
			if len(engine.builds) != 0 {
				t.Fatalf("engine builds = %+v, want none for a refused layer", engine.builds)
			}
		})
	}
	t.Run("declared media type and size", func(t *testing.T) {
		engine := fakeEngineForImages()
		sn := newStagingSnapshotter(t, engine)
		root, chains := stageImage(t, sn, layers...)
		if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		desc := ocispec.Descriptor{Digest: layerBlob(layers[0]), MediaType: ocispec.MediaTypeImageLayerGzip, Size: layerSize}
		if _, err := sn.Apply(ctx, desc, stagingMount("extract")); err != nil {
			t.Fatalf("Apply() error = %v, want the manifest's own descriptor accepted", err)
		}
	})
}

// TestApplyDeclinesUnanchoredStagedChain covers the labels a conversion is
// anchored on: without the manifest digest there is nothing to build against,
// and without a namespace there is no content store to read. containerd
// aborts the pull on the failed apply, so no commit follows.
func TestApplyDeclinesUnanchoredStagedChain(t *testing.T) {
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
			if _, err := sn.Apply(tt.ctx, ocispec.Descriptor{Digest: digest.FromString("blob")}, stagingMount("extract")); err == nil || !tt.wantErr(err) {
				t.Fatalf("Apply() error = %v, want a declined conversion", err)
			}
			if len(engine.builds) != 0 {
				t.Fatalf("engine builds = %+v, want none", engine.builds)
			}
		})
	}
}

// TestApplyPropagatesBuildFailure covers a layer the build cannot take and an
// image it cannot finish: the apply fails, so the runtime aborts the pull,
// and the build is dropped rather than left waiting for layers that will not
// come.
func TestApplyPropagatesBuildFailure(t *testing.T) {
	ctx := pullNamespace()
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	buildErr := errors.New("build failed")
	tests := []struct {
		name      string
		layer     int
		fail      func(*fakeEngine)
		wantCalls []string
	}{
		{name: "ingest", layer: 0, fail: func(e *fakeEngine) { e.ingestErr = buildErr }, wantCalls: []string{"build0.ingest(0)", "build0.close"}},
		{name: "finish", layer: 1, fail: func(e *fakeEngine) { e.finishErr = buildErr }, wantCalls: []string{"build0.finish"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := fakeEngineForImages()
			tt.fail(engine)
			sn := newStagingSnapshotter(t, engine)
			root, chains := stageImage(t, sn, layers...)
			if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[tt.layer]))); err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if err := applyStaged(ctx, sn, "extract", layers[tt.layer]); !errors.Is(err, buildErr) {
				t.Fatalf("Apply() error = %v, want %v", err, buildErr)
			}
			if !slices.Equal(engine.calls, tt.wantCalls) {
				t.Fatalf("engine calls = %v, want %v", engine.calls, tt.wantCalls)
			}
			if len(sn.builds) != 0 {
				t.Fatalf("open builds = %d, want none after the failure", len(sn.builds))
			}
		})
	}
}

// TestCommitStagedRecordReadsNothing covers the commits of a staged pull,
// which containerd makes inside its own metadata transaction: a non-leaf
// commit files the record as it stands, still staging, with no build and no
// content read; the leaf commit pins the image from what the differ left on
// the record — no build open, no content store to read, so a restart between
// the leaf's Apply and its Commit is survived; and a leaf the differ never
// applied is filed as a staging record and pins nothing.
func TestCommitStagedRecordReadsNothing(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	root, chains := stageImage(t, sn, layers...)
	content := sn.blobs.client

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	sn.blobs.client = fakeContent{blobs: map[digest.Digest][]byte{}}
	base := "moby/1/" + chains[0].String()
	if err := sn.Commit(ctx, base, "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	info, err := sn.Stat(ctx, base)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Labels[stagingLabel] == "" || info.Labels[chainMemberLabel] != "" {
		t.Fatalf("committed labels = %v, want a staging record", info.Labels)
	}

	sn.blobs.client = content
	if _, err := sn.Prepare(ctx, "extract-leaf", base, snapshots.WithLabels(stagedLabels("app:v1", root, chains[1]))); err != nil {
		t.Fatalf("Prepare(leaf) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract-leaf", layers[1]); err != nil {
		t.Fatalf("Apply(leaf) error = %v", err)
	}
	if len(sn.builds) != 0 {
		t.Fatalf("open builds = %d, want none after the leaf's apply", len(sn.builds))
	}
	sn.blobs.client = fakeContent{blobs: map[digest.Digest][]byte{}}
	leaf := "moby/2/" + chains[1].String()
	if err := sn.Commit(ctx, leaf, "extract-leaf"); err != nil {
		t.Fatalf("Commit(leaf) error = %v", err)
	}
	want := []string{"build0.finish"}
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v: no build touched by a commit", engine.calls, want)
	}
	holds := []holdCall{{ref: root.String(), holder: buildHolder("moby", "extract-leaf")}}
	if !slices.Equal(engine.holds, holds) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, holds)
	}

	sn.blobs.client = content
	if _, err := sn.Prepare(ctx, "extract-unapplied", base, snapshots.WithLabels(stagedLabels("app:v1", root, chains[1]))); err != nil {
		t.Fatalf("Prepare(unapplied) error = %v", err)
	}
	if err := sn.Commit(ctx, "moby/3/"+chains[1].String(), "extract-unapplied"); err != nil {
		t.Fatalf("Commit(unapplied) error = %v", err)
	}
	if !slices.Equal(engine.holds, holds) {
		t.Fatalf("engine Hold calls = %+v, want no hold for a leaf the differ never applied", engine.holds)
	}
}

// TestCommitConvergesOnDuplicateLeaves covers two pulls of the same image
// racing to commit the chain: each build holds under its own holder, only one
// record survives, and it carries the holder of the build whose commit won.
func TestCommitConvergesOnDuplicateLeaves(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1")}
	root, chains := stageImage(t, sn, layers...)
	name := "moby/1/" + chains[0].String()

	for _, key := range []string{"extract-a", "extract-b"} {
		if _, err := sn.Prepare(ctx, key, "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if err := applyStaged(ctx, sn, key, layers[0]); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
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
	// Each build held when it finished; a commit neither holds nor releases,
	// so the loser's hold stands until its staging record is removed.
	holders := []string{buildHolder("moby", "extract-a"), buildHolder("moby", "extract-b")}
	want := []holdCall{{ref: root.String(), holder: holders[0]}, {ref: root.String(), holder: holders[1]}}
	if !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}
	if len(engine.releases) != 0 {
		t.Fatalf("engine Release calls = %+v, want the surviving record still held", engine.releases)
	}
	info, err := sn.Stat(ctx, name)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", name, err)
	}
	if info.Labels[rootDigestLabel] != root.String() || !slices.Contains(holders, info.Labels[holderLabel]) {
		t.Fatalf("committed labels = %v, want the built image's digest under one of %v", info.Labels, holders)
	}
}

// TestPrepareReadmissionKeepsHold covers two runtimes handshaking over the same
// leaf chain: containerd names that record by the chain alone, so the second
// admission finds the record already there and must leave its hold alone.
func TestPrepareReadmissionKeepsHold(t *testing.T) {
	ctx := context.Background()
	leaf := digest.FromString("leaf-chain")
	image := testImageInfo("image", leaf)
	engine := fakeEngineForImages(image)
	sn := newSnapshotterForTest(t, engine, notMounted)

	seedChainRecord(t, ctx, sn, leaf.String(), "", image.Root.String())
	seedChainRecord(t, ctx, sn, leaf.String(), "", image.Root.String())

	held := holdCall{ref: image.Root.String(), holder: recordHolder(leaf.String())}
	if want := []holdCall{held, held}; !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}
	if len(engine.releases) != 0 {
		t.Fatalf("engine Release calls = %+v, want the admitted image still held", engine.releases)
	}
}

// TestRemoveOfStagingLeafReleasesBuildHold covers a finished image whose leaf
// record never lands: the commit fails, and the hold the build took stays
// with the staging record, which releases it when the runtime's unwinding
// removes the record.
func TestRemoveOfStagingLeafReleasesBuildHold(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1")}
	root, chains := stageImage(t, sn, layers...)
	name := "moby/1/" + chains[0].String()

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	// The record's options are applied inside the write; one that fails is a
	// write failure arriving after the image was held.
	recordErr := errors.New("record failed")
	failing := func(*snapshots.Info) error { return recordErr }
	if err := sn.Commit(ctx, name, "extract", failing); !errors.Is(err, recordErr) {
		t.Fatalf("Commit() error = %v, want %v", err, recordErr)
	}
	want := []holdCall{{ref: root.String(), holder: buildHolder("moby", "extract")}}
	if !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}
	if len(engine.releases) != 0 {
		t.Fatalf("engine Release calls = %+v, want none from a failed commit", engine.releases)
	}

	if err := sn.Remove(ctx, "extract"); err != nil {
		t.Fatalf("Remove(staging leaf) error = %v", err)
	}
	if !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls = %+v, want the build's hold given back", engine.releases)
	}
}

// TestRemoveKeepsImageHeldForOtherRecords covers two runtimes pulling one image
// into their own namespaces: containerd names a record per namespace, each
// takes its own hold, and removing one leaves the other's standing.
func TestRemoveKeepsImageHeldForOtherRecords(t *testing.T) {
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1")}
	root, chains := stageImage(t, sn, layers...)

	names := make([]string, 2)
	for i, ns := range []string{"moby", "k8s.io"} {
		ctx := namespaces.WithNamespace(context.Background(), ns)
		names[i] = ns + "/1/" + chains[0].String()
		if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
			t.Fatalf("Prepare() in %s error = %v", ns, err)
		}
		if err := applyStaged(ctx, sn, "extract", layers[0]); err != nil {
			t.Fatalf("Apply() in %s error = %v", ns, err)
		}
		if err := sn.Commit(ctx, names[i], "extract"); err != nil {
			t.Fatalf("Commit(%s) error = %v", names[i], err)
		}
	}
	want := []holdCall{
		{ref: root.String(), holder: buildHolder("moby", "extract")},
		{ref: root.String(), holder: buildHolder("k8s.io", "extract")},
	}
	if !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want one per pull", engine.holds)
	}

	if err := sn.Remove(namespaces.WithNamespace(context.Background(), "moby"), names[0]); err != nil {
		t.Fatalf("Remove(%s) error = %v", names[0], err)
	}
	if !slices.Equal(engine.releases, want[:1]) {
		t.Fatalf("engine Release calls = %+v, want only the removed record's hold", engine.releases)
	}
}

// TestApplySeparatesBuildsByNamespace covers the same image pulled into two
// namespaces at once: each runtime's layers live in its own content store,
// so each pull gets a build of its own, and each finishes and pins the image
// under its own leaf record.
func TestApplySeparatesBuildsByNamespace(t *testing.T) {
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	root, chains := stageImage(t, sn, layers...)

	for i, ns := range []string{"moby", "k8s.io"} {
		ctx := namespaces.WithNamespace(context.Background(), ns)
		key := ns + "/1/extract"
		if _, err := sn.Prepare(ctx, key, "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if err := applyStaged(ctx, sn, key, layers[0]); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
		if want := fmt.Sprintf("build%d.ingest(0)", i); engine.calls[len(engine.calls)-1] != want {
			t.Fatalf("engine calls = %v, want %s last", engine.calls, want)
		}
	}
	if len(sn.builds) != 2 {
		t.Fatalf("open builds = %d, want one per namespace", len(sn.builds))
	}

	for i, ns := range []string{"moby", "k8s.io"} {
		ctx := namespaces.WithNamespace(context.Background(), ns)
		base, leaf := ns+"/1/"+chains[0].String(), ns+"/2/"+chains[1].String()
		if err := sn.Commit(ctx, base, ns+"/1/extract"); err != nil {
			t.Fatalf("Commit(%s) error = %v", base, err)
		}
		if _, err := sn.Prepare(ctx, ns+"/2/extract", base, snapshots.WithLabels(stagedLabels("app:v1", root, chains[1]))); err != nil {
			t.Fatalf("Prepare(%s leaf) error = %v", ns, err)
		}
		if err := applyStaged(ctx, sn, ns+"/2/extract", layers[1]); err != nil {
			t.Fatalf("Apply(%s leaf) error = %v", ns, err)
		}
		if want := fmt.Sprintf("build%d.finish", i); engine.calls[len(engine.calls)-1] != want {
			t.Fatalf("engine calls = %v, want %s last", engine.calls, want)
		}
		if err := sn.Commit(ctx, leaf, ns+"/2/extract"); err != nil {
			t.Fatalf("Commit(%s) error = %v", leaf, err)
		}
	}
	want := []holdCall{
		{ref: root.String(), holder: buildHolder("moby", "moby/1/extract")},
		{ref: root.String(), holder: buildHolder("k8s.io", "k8s.io/1/extract")},
	}
	if !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}
	if len(sn.builds) != 0 {
		t.Fatalf("open builds = %d, want none after both leaves", len(sn.builds))
	}
}

// TestApplyResumesAfterRestart covers a pull interrupted by a daemon restart
// between a layer's Apply and its Commit: the record is committed with no
// build open, and the next layer's Apply starts a build that catches up on
// the committed layers from the content store.
func TestApplyResumesAfterRestart(t *testing.T) {
	ctx := pullNamespace()
	root := t.TempDir()
	engine := fakeEngineForImages()
	engine.objectsDir = t.TempDir()
	first := newConfiguredSnapshotter(t, Config{Root: root, AdmitAny: true}, engine, notMounted)
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	manifest, chains := stageImage(t, first, layers...)

	if _, err := first.Prepare(ctx, "moby/1/extract", "", snapshots.WithLabels(stagedLabels("app:v1", manifest, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, first, "moby/1/extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	want := []string{"build0.ingest(0)", "build0.close"}
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v: the restart abandons the build", engine.calls, want)
	}

	second := newConfiguredSnapshotter(t, Config{Root: root, AdmitAny: true}, engine, notMounted)
	second.blobs.client = first.blobs.client
	base := "moby/1/" + chains[0].String()
	if err := second.Commit(ctx, base, "moby/1/extract"); err != nil {
		t.Fatalf("Commit() after restart error = %v", err)
	}
	if _, err := second.Prepare(ctx, "moby/2/extract", base, snapshots.WithLabels(stagedLabels("app:v1", manifest, chains[1]))); err != nil {
		t.Fatalf("Prepare(leaf) error = %v", err)
	}
	if err := applyStaged(ctx, second, "moby/2/extract", layers[1]); err != nil {
		t.Fatalf("Apply(leaf) error = %v", err)
	}
	leaf := "moby/2/" + chains[1].String()
	if err := second.Commit(ctx, leaf, "moby/2/extract"); err != nil {
		t.Fatalf("Commit(leaf) error = %v", err)
	}
	want = append(want, "build1.finish")
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v", engine.calls, want)
	}
}

// TestApplyFinishClosesSameLeafBuilds covers two images built from the same
// layers under different manifests, pulled together: they share every chain
// record, so only one pull applies the last layer, and finishing that pull's
// build must also let go of the other's, which nothing would finish.
func TestApplyFinishClosesSameLeafBuilds(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	rootA, chains := stageImage(t, sn, layers...)
	rootB, _ := stageImageBy(t, sn, "other", layers...)
	base := "moby/1/" + chains[0].String()

	// The first pull applies and commits the shared chain; the second, finding
	// it present, goes straight on to the leaf.
	if _, err := sn.Prepare(ctx, "moby/1/extract-a", "", snapshots.WithLabels(stagedLabels("a:v1", rootA, chains[0]))); err != nil {
		t.Fatalf("Prepare(extract-a) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/1/extract-a", layers[0]); err != nil {
		t.Fatalf("Apply(extract-a) error = %v", err)
	}
	if err := sn.Commit(ctx, base, "moby/1/extract-a"); err != nil {
		t.Fatalf("Commit(extract-a) error = %v", err)
	}
	if _, err := sn.Prepare(ctx, "moby/2/extract-b", base, snapshots.WithLabels(stagedLabels("b:v1", rootB, chains[1]))); err != nil {
		t.Fatalf("Prepare(extract-b) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/2/extract-b", layers[1]); err != nil {
		t.Fatalf("Apply(extract-b) error = %v", err)
	}
	want := []string{"build0.ingest(0)", "build1.finish", "build0.close"}
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v", engine.calls, want)
	}
	if len(sn.builds) != 0 {
		t.Fatalf("open builds = %d, want none after the leaf", len(sn.builds))
	}
}

// stagedStep runs one layer of a labeled staged pull — Prepare, Apply and
// Commit — the way containerd does, and returns the committed name.
func stagedStep(t *testing.T, sn *snapshotter, key, parent, imageRef string, root, chain, layer digest.Digest) string {
	t.Helper()
	ctx := pullNamespace()
	labels := snapshots.WithLabels(stagedLabels(imageRef, root, chain))
	if _, err := sn.Prepare(ctx, key, parent, labels); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	if err := applyStaged(ctx, sn, key, layer); err != nil {
		t.Fatalf("Apply(%s) error = %v", key, err)
	}
	name := key[:strings.LastIndex(key, "/")+1] + chain.String()
	if err := sn.Commit(ctx, name, key, labels); err != nil {
		t.Fatalf("Commit(%s) error = %v", name, err)
	}
	return name
}

// TestApplyAdoptsStoredBase covers a staged pull of an image derived from a
// base the store already holds: the layer at the base's whole chain is
// adopted — its record holds the base under the build holder of its key and
// carries the leaf markers before the Apply returns, while the derived
// image's build goes on ingesting it — so its Commit files a chain member, a
// container over it mounts the base with no promotion, and removing the
// record releases the base.
func TestApplyAdoptsStoredBase(t *testing.T) {
	ctx := pullNamespace()
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	rootB, chains := stageImageBy(t, sn, "derived", x, y, w)
	base := testImageInfo("base", chains[0], chains[1])
	addFakeImage(engine, base)
	const ref = "registry.example/b:v1"

	first := stagedStep(t, sn, "moby/0/extract", "", ref, rootB, chains[0], x)
	labels := snapshots.WithLabels(stagedLabels(ref, rootB, chains[1]))
	if _, err := sn.Prepare(ctx, "moby/1/extract", first, labels); err != nil {
		t.Fatalf("Prepare(extract) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/1/extract", y); err != nil {
		t.Fatalf("Apply(extract) error = %v", err)
	}
	holder := buildHolder("moby", "moby/1/extract")
	info, err := sn.Stat(ctx, "moby/1/extract")
	if err != nil {
		t.Fatalf("Stat(extract) error = %v", err)
	}
	if info.Labels[leafLabel] == "" || info.Labels[rootDigestLabel] != base.Root.String() || info.Labels[holderLabel] != holder {
		t.Fatalf("adopted record labels = %v, want the leaf markers naming %s under %s", info.Labels, base.Root, holder)
	}
	if want := []holdCall{{ref: base.Root.String(), holder: holder}}; !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}
	if want := []string{"build0.ingest(0)", "hold", "build0.ingest(1)"}; !slices.Equal(engine.callLog(), want) {
		t.Fatalf("engine calls = %v, want %v: the derived build ingests the adopted layer too", engine.callLog(), want)
	}
	second := "moby/1/" + chains[1].String()
	if err := sn.Commit(ctx, second, "moby/1/extract", labels); err != nil {
		t.Fatalf("Commit(%s) error = %v", second, err)
	}
	info, err = sn.Stat(ctx, second)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", second, err)
	}
	if info.Labels[chainMemberLabel] == "" || info.Labels[rootDigestLabel] != base.Root.String() || info.Labels[holderLabel] != holder {
		t.Fatalf("committed record labels = %v, want a chain member holding %s under %s", info.Labels, base.Root, holder)
	}
	leaf := stagedStep(t, sn, "moby/2/extract", second, ref, rootB, chains[2], w)
	if len(engine.builds) != 1 || engine.lists != 2 {
		t.Fatalf("engine builds = %+v, lists = %d, want the derived build alone and one store scan per layer below the leaf", engine.builds, engine.lists)
	}

	var calls []mountCall
	recordMounts(engine, &calls)
	mounts, err := sn.Prepare(ctx, "moby/9/container-init", second)
	if err != nil {
		t.Fatalf("Prepare(container-init) error = %v", err)
	}
	if len(calls) != 1 || calls[0].chainID != chains[1].String() {
		t.Fatalf("MountChain calls = %+v, want the base chain %s", calls, chains[1])
	}
	if got := optionValue(t, mounts[0], "lowerdir"); got != calls[0].target+"::"+engine.objectsDir {
		t.Fatalf("lowerdir = %q, want the base image only", got)
	}
	if len(engine.builds) != 1 || imagesOf(sn).listCount() != 0 {
		t.Fatalf("engine builds = %+v, image listings = %d, want nothing promoted", engine.builds, imagesOf(sn).listCount())
	}

	if err := sn.Remove(ctx, "moby/9/container-init"); err != nil {
		t.Fatalf("Remove(container-init) error = %v", err)
	}
	if err := sn.Remove(ctx, leaf); err != nil {
		t.Fatalf("Remove(%s) error = %v", leaf, err)
	}
	if err := sn.Remove(ctx, second); err != nil {
		t.Fatalf("Remove(%s) error = %v", second, err)
	}
	want := []holdCall{{ref: rootB.String(), holder: buildHolder("moby", "moby/0/extract")}, {ref: base.Root.String(), holder: holder}}
	if !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls = %+v, want %+v", engine.releases, want)
	}
}

// TestApplyLeavesUnstoredChainStaged covers the store holding no image
// ending in a staged layer's chain: the record stays a plain staging record
// and nothing is held.
func TestApplyLeavesUnstoredChainStaged(t *testing.T) {
	ctx := pullNamespace()
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages(testImageInfo("other", digest.FromString("other-chain")))
	sn := newStagingSnapshotter(t, engine)
	rootB, chains := stageImageBy(t, sn, "derived", x, y, w)
	const ref = "registry.example/b:v1"

	first := stagedStep(t, sn, "moby/0/extract", "", ref, rootB, chains[0], x)
	if _, err := sn.Prepare(ctx, "moby/1/extract", first, snapshots.WithLabels(stagedLabels(ref, rootB, chains[1]))); err != nil {
		t.Fatalf("Prepare(extract) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/1/extract", y); err != nil {
		t.Fatalf("Apply(extract) error = %v", err)
	}
	info, err := sn.Stat(ctx, "moby/1/extract")
	if err != nil {
		t.Fatalf("Stat(extract) error = %v", err)
	}
	if info.Labels[leafLabel] != "" || info.Labels[rootDigestLabel] != "" || info.Labels[holderLabel] != "" {
		t.Fatalf("record labels = %v, want no leaf markers", info.Labels)
	}
	if len(engine.holds) != 0 || engine.lists != 2 {
		t.Fatalf("engine holds = %+v, lists = %d, want nothing held after one store scan per layer", engine.holds, engine.lists)
	}
}

// TestApplyAdoptionRevalidates covers a stored image removed between the
// store scan and the hold: the hold lands, the second look finds another
// chain, and the hold is released on an uncancelled context, a collection
// is asked for, and the record stays a plain staging record while the pull
// goes on.
func TestApplyAdoptionRevalidates(t *testing.T) {
	ctx := pullNamespace()
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	rootB, chains := stageImageBy(t, sn, "derived", x, y, w)
	base := testImageInfo("base", chains[0], chains[1])
	replaced := testImageInfo("base", chains[0], digest.FromString("replaced-chain"))
	engine.inspectFunc = func(_ context.Context, ref string) (*ocicfs.ImageInfo, error) {
		if ref != base.Root.String() {
			return nil, fmt.Errorf("inspect %s: %w", ref, ocicfs.ErrNotFound)
		}
		engine.mu.Lock()
		defer engine.mu.Unlock()
		if len(engine.holds) > 0 {
			return replaced, nil
		}
		return base, nil
	}
	addFakeImage(engine, base)
	const ref = "registry.example/b:v1"

	first := stagedStep(t, sn, "moby/0/extract", "", ref, rootB, chains[0], x)
	if _, err := sn.Prepare(ctx, "moby/1/extract", first, snapshots.WithLabels(stagedLabels(ref, rootB, chains[1]))); err != nil {
		t.Fatalf("Prepare(extract) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/1/extract", y); err != nil {
		t.Fatalf("Apply(extract) error = %v", err)
	}
	holder := buildHolder("moby", "moby/1/extract")
	want := []holdCall{{ref: base.Root.String(), holder: holder}}
	if !slices.Equal(engine.holds, want) || !slices.Equal(engine.releases, want) {
		t.Fatalf("engine holds = %+v, releases = %+v, want the vanished image held once and released once", engine.holds, engine.releases)
	}
	if engine.releaseCtxErrs[0] != nil {
		t.Fatalf("Release context error = %v, want none", engine.releaseCtxErrs[0])
	}
	awaitGCs(t, engine, 2)
	info, err := sn.Stat(ctx, "moby/1/extract")
	if err != nil {
		t.Fatalf("Stat(extract) error = %v", err)
	}
	if info.Labels[leafLabel] != "" || info.Labels[rootDigestLabel] != "" || info.Labels[holderLabel] != "" {
		t.Fatalf("record labels = %v, want no leaf markers", info.Labels)
	}
	if calls := engine.callLog(); calls[len(calls)-1] != "build0.ingest(1)" {
		t.Fatalf("engine calls = %v, want the derived build to have ingested the layer", calls)
	}
}

// TestApplyAdoptionRetiresBaseBuild covers a pull of the base itself in
// progress when a derived pull adopts the stored base at their shared chain:
// the base pull's build, which nothing will finish once the chain member is
// committed, is closed, and the derived pull's build is not.
func TestApplyAdoptionRetiresBaseBuild(t *testing.T) {
	ctx := pullNamespace()
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	rootA, chainsA := stageImageBy(t, sn, "base", x, y)
	rootB, chains := stageImageBy(t, sn, "derived", x, y, w)
	addFakeImage(engine, testImageInfo("stored-base", chains[0], chains[1]))

	first := stagedStep(t, sn, "moby/0/extract", "", "registry.example/a:v1", rootA, chainsA[0], x)
	if _, err := sn.Prepare(ctx, "moby/1/extract", first, snapshots.WithLabels(stagedLabels("registry.example/b:v1", rootB, chains[1]))); err != nil {
		t.Fatalf("Prepare(extract) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/1/extract", y); err != nil {
		t.Fatalf("Apply(extract) error = %v", err)
	}
	want := []string{"build0.ingest(0)", "hold", "build0.close", "build1.ingest(1)"}
	if !slices.Equal(engine.callLog(), want) {
		t.Fatalf("engine calls = %v, want %v: the base pull's build closed, the derived pull's ingesting", engine.callLog(), want)
	}
	if _, ok := sn.builds[buildKey{namespace: "moby", root: rootB.String()}]; !ok || len(sn.builds) != 1 {
		t.Fatalf("open builds = %v, want the derived image's alone", sn.builds)
	}
	awaitGCs(t, engine, 2)
}

// TestApplyAdoptionReleasesOnHoldFailure covers a hold that fails after
// its marker may already be written: the hold is released on an uncancelled
// context, a collection is asked for, the Apply fails and the record is left
// as it was.
func TestApplyAdoptionReleasesOnHoldFailure(t *testing.T) {
	ctx := pullNamespace()
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	rootB, chains := stageImageBy(t, sn, "derived", x, y, w)
	base := testImageInfo("base", chains[0], chains[1])
	addFakeImage(engine, base)
	const ref = "registry.example/b:v1"

	first := stagedStep(t, sn, "moby/0/extract", "", ref, rootB, chains[0], x)
	if _, err := sn.Prepare(ctx, "moby/1/extract", first, snapshots.WithLabels(stagedLabels(ref, rootB, chains[1]))); err != nil {
		t.Fatalf("Prepare(extract) error = %v", err)
	}
	before, err := sn.Stat(ctx, "moby/1/extract")
	if err != nil {
		t.Fatalf("Stat(extract) error = %v", err)
	}
	holdErr := errors.New("hold marker not synced")
	engine.holdErr = holdErr
	if err := applyStaged(ctx, sn, "moby/1/extract", y); !errors.Is(err, holdErr) {
		t.Fatalf("Apply(extract) error = %v, want %v", err, holdErr)
	}
	holder := buildHolder("moby", "moby/1/extract")
	want := []holdCall{{ref: base.Root.String(), holder: holder}}
	if !slices.Equal(engine.holds, want) || !slices.Equal(engine.releases, want) {
		t.Fatalf("engine holds = %+v, releases = %+v, want the failed hold released", engine.holds, engine.releases)
	}
	if engine.releaseCtxErrs[0] != nil {
		t.Fatalf("Release context error = %v, want none", engine.releaseCtxErrs[0])
	}
	awaitGCs(t, engine, 2)
	after, err := sn.Stat(ctx, "moby/1/extract")
	if err != nil {
		t.Fatalf("Stat(extract) error = %v", err)
	}
	if !maps.Equal(after.Labels, before.Labels) {
		t.Fatalf("record labels = %v, want unchanged %v", after.Labels, before.Labels)
	}
}

// TestApplyAdoptionTriesNextCandidate covers two stored images ending in
// the chain where the first, by root, vanishes under its hold: it is
// released and the second is adopted.
func TestApplyAdoptionTriesNextCandidate(t *testing.T) {
	ctx := pullNamespace()
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	rootB, chains := stageImageBy(t, sn, "derived", x, y, w)
	one := testImageInfo("base-one", chains[0], chains[1])
	two := testImageInfo("base-two", chains[0], chains[1])
	addFakeImage(engine, one)
	addFakeImage(engine, two)
	roots := []string{one.Root.String(), two.Root.String()}
	slices.Sort(roots)
	first, second := roots[0], roots[1]
	engine.inspectFunc = func(_ context.Context, ref string) (*ocicfs.ImageInfo, error) {
		info, ok := engine.infos[ref]
		if !ok {
			return nil, fmt.Errorf("inspect %s: %w", ref, ocicfs.ErrNotFound)
		}
		engine.mu.Lock()
		defer engine.mu.Unlock()
		if ref == first && len(engine.holds) > 0 {
			return nil, fmt.Errorf("inspect %s: %w", ref, ocicfs.ErrNotFound)
		}
		return info, nil
	}
	const ref = "registry.example/b:v1"

	base := stagedStep(t, sn, "moby/0/extract", "", ref, rootB, chains[0], x)
	if _, err := sn.Prepare(ctx, "moby/1/extract", base, snapshots.WithLabels(stagedLabels(ref, rootB, chains[1]))); err != nil {
		t.Fatalf("Prepare(extract) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/1/extract", y); err != nil {
		t.Fatalf("Apply(extract) error = %v", err)
	}
	holder := buildHolder("moby", "moby/1/extract")
	if want := []holdCall{{ref: first, holder: holder}, {ref: second, holder: holder}}; !slices.Equal(engine.holds, want) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, want)
	}
	if want := []holdCall{{ref: first, holder: holder}}; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls = %+v, want the vanished candidate's hold alone", engine.releases)
	}
	info, err := sn.Stat(ctx, "moby/1/extract")
	if err != nil {
		t.Fatalf("Stat(extract) error = %v", err)
	}
	if info.Labels[leafLabel] == "" || info.Labels[rootDigestLabel] != second || info.Labels[holderLabel] != holder {
		t.Fatalf("record labels = %v, want the leaf markers naming %s under %s", info.Labels, second, holder)
	}
	awaitGCs(t, engine, 2)
}

// TestReconcileHoldsByRootAndHolder covers a stored base adopted at the
// first layer of a derived pull: the adopted record and the pull's build
// hold two roots under one holder name. Removing the derived leaf with its
// release failing strands the derived image's hold; the restart releases
// that hold, which no record names by root and holder, and keeps the
// base's, which the adopted record does.
func TestReconcileHoldsByRootAndHolder(t *testing.T) {
	ctx := pullNamespace()
	x, y := digest.FromString("layer-x"), digest.FromString("layer-y")
	root := t.TempDir()
	engine := fakeEngineForImages()
	engine.objectsDir = t.TempDir()
	sn := newConfiguredSnapshotter(t, Config{Root: root, AdmitAny: true}, engine, notMounted)
	rootB, chains := stageImageBy(t, sn, "derived", x, y)
	base := testImageInfo("base", chains[0])
	addFakeImage(engine, base)
	const ref = "registry.example/b:v1"

	first := stagedStep(t, sn, "moby/0/extract", "", ref, rootB, chains[0], x)
	leaf := stagedStep(t, sn, "moby/1/extract", first, ref, rootB, chains[1], y)
	holder := buildHolder("moby", "moby/0/extract")
	wantHolds := []holdCall{{ref: base.Root.String(), holder: holder}, {ref: rootB.String(), holder: holder}}
	if !slices.Equal(engine.holds, wantHolds) {
		t.Fatalf("engine Hold calls = %+v, want %+v: two roots under one holder", engine.holds, wantHolds)
	}
	engine.releaseErr = errors.New("release failed")
	if err := sn.Remove(ctx, leaf); err != nil {
		t.Fatalf("Remove(%s) error = %v", leaf, err)
	}
	engine.releaseErr = nil
	engine.held = map[digest.Digest][]string{base.Root: {holder}, rootB: {holder}}
	if err := sn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	engine.releases = nil
	newConfiguredSnapshotter(t, Config{Root: root, AdmitAny: true}, engine, notMounted)
	if want := []holdCall{{ref: rootB.String(), holder: holder}}; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls at restart = %+v, want the derived image's stranded hold alone", engine.releases)
	}
}

// TestReconcileHoldsOnEmptyRecordStore covers a start on a root with no
// records while the ocicfs store still holds images under this
// snapshotter's holder names: every such hold is released and holds under
// other holders are kept.
func TestReconcileHoldsOnEmptyRecordStore(t *testing.T) {
	ours, foreign := digest.FromString("ours"), digest.FromString("foreign")
	holder := buildHolder("moby", "moby/1/extract")
	engine := fakeEngineForImages()
	engine.held = map[digest.Digest][]string{ours: {holder}, foreign: {"agent"}}

	newConfiguredSnapshotter(t, Config{Root: t.TempDir()}, engine, notMounted)
	if want := []holdCall{{ref: ours.String(), holder: holder}}; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls at start = %+v, want %+v", engine.releases, want)
	}
}

// TestRemoveClosesAbortedPullBuild covers the runtime aborting a pull: the
// active staging snapshot is removed on the garbage collector's context,
// which names no namespace, under the namespaced key containerd files backend
// records by, and the pull's build goes with it. A retried pull then finds no
// build at its next layer and starts one that catches up.
func TestRemoveClosesAbortedPullBuild(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2"), digest.FromString("layer-3")}
	root, chains := stageImage(t, sn, layers...)
	base := "moby/1/" + chains[0].String()

	if _, err := sn.Prepare(ctx, "moby/1/extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare(moby/1/extract) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/1/extract", layers[0]); err != nil {
		t.Fatalf("Apply(moby/1/extract) error = %v", err)
	}
	if err := sn.Commit(ctx, base, "moby/1/extract"); err != nil {
		t.Fatalf("Commit(%s) error = %v", base, err)
	}
	if _, err := sn.Prepare(ctx, "moby/2/extract", base, snapshots.WithLabels(stagedLabels("app:v1", root, chains[1]))); err != nil {
		t.Fatalf("Prepare(moby/2/extract) error = %v", err)
	}
	if err := sn.Remove(context.Background(), "moby/2/extract"); err != nil {
		t.Fatalf("Remove(moby/2/extract) error = %v", err)
	}
	want := []string{"build0.ingest(0)", "build0.close"}
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v", engine.calls, want)
	}

	if _, err := sn.Prepare(ctx, "moby/3/extract", base, snapshots.WithLabels(stagedLabels("app:v1", root, chains[1]))); err != nil {
		t.Fatalf("Prepare(moby/3/extract) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/3/extract", layers[1]); err != nil {
		t.Fatalf("Apply(moby/3/extract) error = %v", err)
	}
	want = append(want, "build1.ingest(1)")
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v", engine.calls, want)
	}
}

// TestRemoveClosesCollectedStagedRecordBuild covers the garbage collector
// removing the committed layers an aborted pull left below its active
// snapshot: they carry the unpack's labels, as containerd hands them to
// Commit, so their removal closes the build the same way.
func TestRemoveClosesCollectedStagedRecordBuild(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	root, chains := stageImage(t, sn, layers...)
	base := "moby/1/" + chains[0].String()

	labels := snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))
	if _, err := sn.Prepare(ctx, "moby/1/extract", "", labels); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, sn, "moby/1/extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := sn.Commit(ctx, base, "moby/1/extract", labels); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	waitClean(t, sn)
	if got := engine.gcCount(); got != 1 {
		t.Fatalf("collections before the remove = %d, want the startup one alone", got)
	}
	if err := sn.Remove(context.Background(), base); err != nil {
		t.Fatalf("Remove(%s) error = %v", base, err)
	}
	// The staged record releases no hold, so the count can only move by
	// the abandoned build's own request.
	awaitGCs(t, engine, 2)
	want := []string{"build0.ingest(0)", "build0.close"}
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v", engine.calls, want)
	}
	if len(sn.builds) != 0 {
		t.Fatalf("open builds = %d, want none", len(sn.builds))
	}
}

// TestRemoveLeavesOtherBuildsOpen covers the removals that must not touch a
// build in progress: a finished image's leaf, a record of an image served
// from the store (filed under its bare chain identity), and a staged record
// of another image, whose own build is the one to close.
func TestRemoveLeavesOtherBuildsOpen(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layersA := []digest.Digest{digest.FromString("a-1"), digest.FromString("a-2")}
	layersB := []digest.Digest{digest.FromString("b-1")}
	layersC := []digest.Digest{digest.FromString("c-1"), digest.FromString("c-2")}
	rootA, chainsA := stageImage(t, sn, layersA...)
	rootB, chainsB := stageImage(t, sn, layersB...)
	rootC, chainsC := stageImage(t, sn, layersC...)
	// containerd hands Commit the labels the unpack attached to Prepare.
	stage := func(key, name string, root, chain, layer digest.Digest) {
		t.Helper()
		labels := snapshots.WithLabels(stagedLabels(root.String(), root, chain))
		if _, err := sn.Prepare(ctx, key, "", labels); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if err := applyStaged(ctx, sn, key, layer); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
		if err := sn.Commit(ctx, name, key, labels); err != nil {
			t.Fatalf("Commit(%s) error = %v", name, err)
		}
	}

	stage("moby/1/extract", "moby/1/"+chainsA[0].String(), rootA, chainsA[0], layersA[0])
	leafB := "moby/2/" + chainsB[0].String()
	stage("moby/2/extract", leafB, rootB, chainsB[0], layersB[0])
	warm := digest.FromString("warm").String()
	commitChainRecord(t, ctx, sn, warm, warm)
	stagedC := "moby/3/" + chainsC[0].String()
	stage("moby/3/extract", stagedC, rootC, chainsC[0], layersC[0])

	for _, key := range []string{leafB, warm, stagedC} {
		if err := sn.Remove(context.Background(), key); err != nil {
			t.Fatalf("Remove(%s) error = %v", key, err)
		}
	}
	want := []string{"build0.ingest(0)", "build1.finish", "build2.ingest(0)", "build2.close"}
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v", engine.calls, want)
	}
	if _, ok := sn.builds[buildKey{namespace: "moby", root: rootA.String()}]; !ok || len(sn.builds) != 1 {
		t.Fatalf("open builds = %v, want only the first image's", sn.builds)
	}
}

// TestCloseAbandonsOpenBuilds covers shutting the snapshotter down mid-pull:
// the builds in progress are let go, so nothing of theirs outlives it.
func TestCloseAbandonsOpenBuilds(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	root, chains := stageImage(t, sn, layers...)

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := sn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	want := []string{"build0.ingest(0)", "build0.close"}
	if !slices.Equal(engine.calls, want) {
		t.Fatalf("engine calls = %v, want %v", engine.calls, want)
	}
	if len(sn.builds) != 0 {
		t.Fatalf("open builds = %d, want none", len(sn.builds))
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
	layers := []digest.Digest{digest.FromString("layer-1")}
	root, chains := stageImage(t, sn, layers...)
	name := "moby/1/" + chains[0].String()

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:latest", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := sn.Commit(ctx, name, "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	// The tag now points at another image; the release must not follow it.
	stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))

	if err := sn.Remove(ctx, name); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	want := []holdCall{{ref: root.String(), holder: buildHolder("moby", "extract")}}
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
	layers := []digest.Digest{digest.FromString("layer-1")}
	root, chains := stageImage(t, sn, layers...)
	name := "moby/1/" + chains[0].String()

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:latest", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := sn.Commit(ctx, name, "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := sn.Remove(ctx, name); err != nil {
		t.Fatalf("Remove() error = %v, want success despite the failed release", err)
	}
	want := []holdCall{{ref: root.String(), holder: buildHolder("moby", "extract")}}
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
	want := []holdCall{{ref: image.Root.String(), holder: recordHolder(leaf.String())}}
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
// an ocicfs remove could then take the content away from.
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
	layers := []digest.Digest{digest.FromString("layer-1"), digest.FromString("layer-2")}
	root, chains := stageImage(t, sn, layers...)
	engine.infos[root.String()] = &ocicfs.ImageInfo{
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
		if err := applyStaged(ctx, sn, key, layers[i]); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
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
	if _, err := sn.blobs.contentClient(); err != nil {
		t.Fatalf("contentClient() error = %v", err)
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

// TestStagingSnapshotIsPlaceholder covers the staging snapshot's shape: a
// record with no directory, whose one mount — from Prepare and from Mounts
// alike — is the placeholder naming the record, and which counts as nothing
// in usage.
func TestStagingSnapshotIsPlaceholder(t *testing.T) {
	ctx := pullNamespace()
	sn := newStagingSnapshotter(t, fakeEngineForImages())
	root, chains := stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))
	imageRef := digest.FromString("unheld-image").String()

	if _, err := sn.Prepare(ctx, "extract-1", "", snapshots.WithLabels(stagedLabels(imageRef, root, chains[0]))); err != nil {
		t.Fatalf("Prepare(extract-1) error = %v", err)
	}
	if err := sn.Commit(ctx, chains[0].String(), "extract-1"); err != nil {
		t.Fatalf("Commit(chain-1) error = %v", err)
	}
	mounts, err := sn.Prepare(ctx, "extract-2", chains[0].String(), snapshots.WithLabels(stagedLabels(imageRef, root, chains[1])))
	if err != nil {
		t.Fatalf("Prepare(extract-2) error = %v", err)
	}
	if !isStagingMount(mounts, "extract-2") {
		t.Fatalf("Prepare() mounts = %v, want the placeholder naming extract-2", mounts)
	}
	if mounts, err := sn.Mounts(ctx, "extract-2"); err != nil || !isStagingMount(mounts, "extract-2") {
		t.Fatalf("Mounts() = %v, %v; want the placeholder naming extract-2", mounts, err)
	}
	if usage, err := sn.Usage(ctx, "extract-2"); err != nil || usage != (snapshots.Usage{}) {
		t.Fatalf("Usage() = %+v, %v; want nothing for a staging record", usage, err)
	}
	entries, err := os.ReadDir(filepath.Join(sn.root, "snapshots"))
	if err != nil {
		t.Fatalf("read snapshots dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("snapshots dir has %d entries for staging records, want none", len(entries))
	}
}

// TestStagingPlaceholderIsUnmountable covers the placeholder doing its job
// against a differ that is not this snapshotter's: containerd's own differ
// temp-mounts the snapshot before extracting into it, and the kernel refuses
// the placeholder's filesystem type, so nothing is ever written anywhere.
func TestStagingPlaceholderIsUnmountable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mounting needs root")
	}
	err := mount.WithTempMount(context.Background(), stagingMount("extract"), func(string) error {
		return errors.New("the placeholder was mounted")
	})
	if !errors.Is(err, unix.ENODEV) {
		t.Fatalf("temp mount of the placeholder error = %v, want ENODEV", err)
	}
}

// TestMountsRefuseStagedAncestor covers a snapshot descending from a staged
// record whose image is not finished: the record is metadata only, like a
// chain member, but the walk must not take it for one and mount an image
// that is not there.
// TestMountsRefuseStagedAncestor covers a container over a staged record
// that stands for no image: the runtime's records name no image ending in
// its chain, so there is nothing to promote it into, and the Prepare fails
// closed without leaving a record behind.
func TestMountsRefuseStagedAncestor(t *testing.T) {
	ctx := pullNamespace()
	sn := newStagingSnapshotter(t, fakeEngineForImages())
	root, chains := stageImage(t, sn, digest.FromString("layer-1"), digest.FromString("layer-2"))
	imagesOf(sn)
	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare(extract) error = %v", err)
	}
	if err := sn.Commit(ctx, chains[0].String(), "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := sn.Prepare(ctx, "container", chains[0].String()); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("Prepare() over a staged record error = %v, want failed precondition", err)
	}
	if _, err := sn.Stat(ctx, "container"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(container) error = %v, want not found", err)
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

// TestPrepareOverImageFollowsStoreVerity covers that the overlay requires
// fs-verity on the store's objects only when the store runs with it: a store
// without it would fail every file read under verity=require.
func TestPrepareOverImageFollowsStoreVerity(t *testing.T) {
	ctx := context.Background()
	chain := digest.FromString("chain-1")
	image := testImageInfo("image-root", chain)
	engine := fakeEngineForImages(image)
	engine.verityOff = true
	sn := newSnapshotterForTest(t, engine, notMounted)
	seedChainRecord(t, ctx, sn, chain.String(), "", image.Root.String())
	var calls []mountCall
	recordMounts(engine, &calls)

	mounts, err := sn.Prepare(ctx, "container", chain.String())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	id, _, err := sn.snapshotInfo(ctx, "container")
	if err != nil {
		t.Fatalf("snapshotInfo(container) error = %v", err)
	}
	dir := sn.getSnapshotDir(id)
	want := []string{
		"workdir=" + filepath.Join(dir, "work"),
		"upperdir=" + filepath.Join(dir, "upper"),
		"lowerdir=" + calls[0].target + "::" + engine.objectsDir,
		"redirect_dir=on",
		"metacopy=on",
	}
	if len(mounts) != 1 || !slices.Equal(mounts[0].Options, want) {
		t.Fatalf("mounts = %v, want overlay without verity=require: %v", mounts, want)
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
	engine.mountChainFunc = func(context.Context, string, string, ...ocicfs.MountOption) error {
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

// TestMountsBaseRecordedByDerivedPull covers a base image whose chain the
// store first met as a prefix of a derived image's: the derived pull records
// every chain as a member, so a container over the base's chain mounts what
// the store holds under it. Nothing, until the base is pulled out of band:
// the Prepare fails naming ocicfs and leaves the record as it was; once the
// base is stored, the same Prepare mounts the base's own image.
func TestMountsBaseRecordedByDerivedPull(t *testing.T) {
	ctx := context.Background()
	chainX := digest.FromString("chain-x")
	chainXY := digest.FromString("chain-xy")
	chainXYW := digest.FromString("chain-xyw")
	derived := testImageInfo("derived", chainX, chainXY, chainXYW)
	engine := fakeEngineForImages(derived)
	sn := newSnapshotterForTest(t, engine, notMounted)
	seedChainRecord(t, ctx, sn, chainX.String(), "", derived.Root.String())
	seedChainRecord(t, ctx, sn, chainXY.String(), chainX.String(), derived.Root.String())
	seedChainRecord(t, ctx, sn, chainXYW.String(), chainXY.String(), derived.Root.String())
	before, err := sn.Stat(ctx, chainXY.String())
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", chainXY, err)
	}

	_, err = sn.Prepare(ctx, "container", chainXY.String())
	if !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), "pull it with ocicfs") {
		t.Fatalf("Prepare() over the unstored base error = %v, want failed precondition naming ocicfs", err)
	}
	if _, err := sn.Stat(ctx, "container"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(container) error = %v, want not found", err)
	}
	if len(engine.builds) != 0 {
		t.Fatalf("engine NewBuild calls = %+v, want none", engine.builds)
	}
	after, err := sn.Stat(ctx, chainXY.String())
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", chainXY, err)
	}
	if !maps.Equal(after.Labels, before.Labels) {
		t.Fatalf("base record labels = %v, want unchanged %v", after.Labels, before.Labels)
	}

	addFakeImage(engine, testImageInfo("base", chainX, chainXY))
	var calls []mountCall
	recordMounts(engine, &calls)
	mounts, err := sn.Prepare(ctx, "container", chainXY.String())
	if err != nil {
		t.Fatalf("Prepare() over the stored base error = %v", err)
	}
	if len(calls) != 1 || calls[0].chainID != chainXY.String() {
		t.Fatalf("MountChain calls = %+v, want the base chain %s", calls, chainXY)
	}
	if got := optionValue(t, mounts[0], "lowerdir"); got != calls[0].target+"::"+engine.objectsDir {
		t.Fatalf("lowerdir = %q, want the base image only", got)
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
	engine.mountChainFunc = func(context.Context, string, string, ...ocicfs.MountOption) error {
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
	infos          map[string]*ocicfs.ImageInfo
	inspectFunc    func(context.Context, string) (*ocicfs.ImageInfo, error)
	mountChainFunc func(context.Context, string, string, ...ocicfs.MountOption) error
	unmountFunc    func(context.Context, string) error
	objectsDir     string
	// verityOff makes VerityRequired report a store running without
	// fs-verity; the zero value reports one running with it.
	verityOff  bool
	holdErr    error
	releaseErr error
	// newBuildErr fails every NewBuild; ingestErr and finishErr fail the
	// corresponding call of every build the engine creates. finishHeldErr
	// fails Finish after the build's hold has been recorded, the way a hold
	// marker written but not synced fails.
	newBuildErr   error
	ingestErr     error
	finishErr     error
	finishHeldErr error
	// listErr fails List, which otherwise lists every image in infos;
	// lists counts the calls.
	listErr error
	lists   int
	// held is what Holds reports; gcErr fails every GC and gcs counts them.
	// gcGate, when set, holds every GC, already counted, until it is closed.
	held   map[digest.Digest][]string
	gcErr  error
	gcs    int
	gcGate chan struct{}
	// finishGate, when set, blocks every build's Finish until the gate is
	// closed; Close closes it and then waits for finishDone, the way a real
	// Close lets a finish already under way complete before it returns.
	finishGate     chan struct{}
	finishDone     chan struct{}
	finishGateOnce sync.Once
	finishDoneOnce sync.Once
	// events records, in order, every hold a build's Finish takes and every
	// Release, so a test can tell which came first. releaseCtxErrs records
	// the state of the context each Release was given.
	events         []string
	releaseCtxErrs []error

	// mu guards the call records, which concurrent commits append to.
	mu sync.Mutex
	// builds records what each NewBuild was asked for; calls records, in
	// order, every call made on those builds ("build0.ingest(1)",
	// "build0.finish", "build1.close") and every hold ("hold").
	builds   []buildCall
	calls    []string
	holds    []holdCall
	releases []holdCall
}

var _ engineAPI = (*fakeEngine)(nil)

func (e *fakeEngine) Inspect(ctx context.Context, ref string) (*ocicfs.ImageInfo, error) {
	if e.inspectFunc != nil {
		return e.inspectFunc(ctx, ref)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, ok := e.infos[ref]
	if !ok {
		return nil, fmt.Errorf("inspect %s: %w", ref, ocicfs.ErrNotFound)
	}
	return info, nil
}

func (e *fakeEngine) List(ctx context.Context) ([]ocicfs.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.listErr != nil {
		return nil, e.listErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lists++
	images := make([]ocicfs.Image, 0, len(e.infos))
	for root := range e.infos {
		images = append(images, ocicfs.Image{Digest: root})
	}
	slices.SortFunc(images, func(a, b ocicfs.Image) int { return strings.Compare(a.Digest, b.Digest) })
	return images, nil
}

func (e *fakeEngine) NewBuild(ctx context.Context, ref string, opts ...ocicfs.PullOption) (ocicfs.Build, error) {
	var pullOpts ocicfs.PullOptions
	for _, opt := range opts {
		opt(&pullOpts)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.newBuildErr != nil {
		return nil, e.newBuildErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.builds = append(e.builds, buildCall{ref: ref, opts: pullOpts})
	return &fakeBuild{engine: e, name: fmt.Sprintf("build%d", len(e.builds)-1), opts: pullOpts}, nil
}

func (e *fakeEngine) record(call string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
}

func (e *fakeEngine) Hold(_ context.Context, ref, holder string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.holds = append(e.holds, holdCall{ref: ref, holder: holder})
	e.calls = append(e.calls, "hold")
	return e.holdErr
}

func (e *fakeEngine) Release(ctx context.Context, ref, holder string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.releases = append(e.releases, holdCall{ref: ref, holder: holder})
	e.events = append(e.events, "release:"+holder)
	e.releaseCtxErrs = append(e.releaseCtxErrs, ctx.Err())
	return e.releaseErr
}

func (e *fakeEngine) Holds(context.Context) (map[digest.Digest][]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.held, nil
}

func (e *fakeEngine) GC(ctx context.Context, _ ...ocicfs.GCOption) (*ocicfs.GCReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.gcs++
	gate, gcErr := e.gcGate, e.gcErr
	e.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if gcErr != nil {
		return nil, gcErr
	}
	return &ocicfs.GCReport{}, nil
}

// setGCGate makes every later collection wait until gate is closed.
func (e *fakeEngine) setGCGate(gate chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gcGate = gate
}

// gcCount reports how many collections the engine has run.
func (e *fakeEngine) gcCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gcs
}

// setGCErr sets the error every later collection fails with.
func (e *fakeEngine) setGCErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gcErr = err
}

// awaitGCs waits for the engine to have run at least n collections.
func awaitGCs(t *testing.T, engine *fakeEngine, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for engine.gcCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("engine ran %d collections, want at least %d", engine.gcCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (e *fakeEngine) MountChain(ctx context.Context, chainID, target string, opts ...ocicfs.MountOption) error {
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
	return fmt.Errorf("mount chain %s: %w", chainID, ocicfs.ErrNotFound)
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

func (e *fakeEngine) VerityRequired() (bool, error) {
	return !e.verityOff, nil
}

func fakeEngineForImages(infos ...*ocicfs.ImageInfo) *fakeEngine {
	engine := &fakeEngine{
		infos:      make(map[string]*ocicfs.ImageInfo, len(infos)),
		objectsDir: "/ocicfs/objects",
	}
	for _, info := range infos {
		addFakeImage(engine, info)
	}
	return engine
}

func addFakeImage(engine *fakeEngine, info *ocicfs.ImageInfo) {
	engine.infos[info.Root.String()] = info
}

type mountCall struct {
	chainID string
	target  string
	opts    ocicfs.MountOptions
}

type buildCall struct {
	ref  string
	opts ocicfs.PullOptions
}

// fakeBuild stands in for an ocicfs image build, recording its calls on the
// engine that created it. A build opened with a hold records that hold, on
// the root it was resolved to, when it finishes, as the real one does.
type fakeBuild struct {
	engine *fakeEngine
	name   string
	opts   ocicfs.PullOptions
}

func (b *fakeBuild) Ingest(_ context.Context, i int) error {
	b.engine.record(fmt.Sprintf("%s.ingest(%d)", b.name, i))
	return b.engine.ingestErr
}

func (b *fakeBuild) Finish(context.Context) (*ocicfs.Image, error) {
	b.engine.record(b.name + ".finish")
	if b.engine.finishGate != nil {
		<-b.engine.finishGate
	}
	if b.engine.finishErr != nil {
		return nil, b.engine.finishErr
	}
	if b.opts.Hold != "" {
		b.engine.mu.Lock()
		b.engine.holds = append(b.engine.holds, holdCall{ref: b.opts.ResolvedRoot.String(), holder: b.opts.Hold})
		b.engine.events = append(b.engine.events, "hold:"+b.opts.Hold)
		b.engine.mu.Unlock()
	}
	if b.engine.finishHeldErr != nil {
		return nil, b.engine.finishHeldErr
	}
	if b.engine.finishDone != nil {
		b.engine.finishDoneOnce.Do(func() { close(b.engine.finishDone) })
	}
	return &ocicfs.Image{}, nil
}

func (b *fakeBuild) Close() {
	b.engine.record(b.name + ".close")
	if b.engine.finishGate != nil {
		b.engine.finishGateOnce.Do(func() { close(b.engine.finishGate) })
	}
	if b.engine.finishDone != nil {
		<-b.engine.finishDone
	}
}

// calls returns a copy of the engine's ordered call log.
func (e *fakeEngine) callLog() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

type holdCall struct {
	ref    string
	holder string
}

// fakeContent serves canned blobs the way containerd's content service does:
// one Read call streams a blob in fixed-size messages, and a blob the store
// does not hold fails the first receive rather than the call; Info reports a
// blob's presence. Anything else panics on the nil embedded client.
type fakeContent struct {
	contentapi.ContentClient
	blobs map[digest.Digest][]byte
}

func (f fakeContent) Info(_ context.Context, req *contentapi.InfoRequest, _ ...grpc.CallOption) (*contentapi.InfoResponse, error) {
	blob, ok := f.blobs[digest.Digest(req.Digest)]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "content %s", req.Digest)
	}
	return &contentapi.InfoResponse{Info: &contentapi.Info{Digest: req.Digest, Size: int64(len(blob))}}, nil
}

// fakeImages serves canned image records the way containerd's images service
// does, recording the namespace header of every List. Anything but List
// panics on the nil embedded client.
type fakeImages struct {
	imagesapi.ImagesClient
	mu     sync.Mutex
	images []*imagesapi.Image
	err    error
	// lists records the namespace header each List call carried.
	lists []string
}

func (f *fakeImages) List(ctx context.Context, _ *imagesapi.ListImagesRequest, _ ...grpc.CallOption) (*imagesapi.ListImagesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	md, _ := metadata.FromOutgoingContext(ctx)
	f.lists = append(f.lists, strings.Join(slices.Compact(md.Get("containerd-namespace")), ","))
	if f.err != nil {
		return nil, f.err
	}
	return &imagesapi.ListImagesResponse{Images: slices.Clone(f.images)}, nil
}

// add registers an image record under name targeting the given descriptor.
func (f *fakeImages) add(name, mediaType string, target digest.Digest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = append(f.images, &imagesapi.Image{
		Name:   name,
		Target: &types.Descriptor{MediaType: mediaType, Digest: target.String()},
	})
}

// listCount reports how many List calls the fake has served.
func (f *fakeImages) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lists)
}

// imagesOf returns the fake images service the snapshotter walks image
// records with, installing an empty one on first use.
func imagesOf(sn *snapshotter) *fakeImages {
	fake, ok := sn.blobs.images.(*fakeImages)
	if !ok {
		fake = &fakeImages{}
		sn.blobs.images = fake
	}
	return fake
}

// contentOf returns the fake content store the snapshotter reads blobs from,
// installing an empty one on first use.
func contentOf(sn *snapshotter) fakeContent {
	store, ok := sn.blobs.client.(fakeContent)
	if !ok {
		store = fakeContent{blobs: map[digest.Digest][]byte{}}
		sn.blobs.client = store
	}
	return store
}

// fakeMessageSize is the size of the messages a fake read streams a blob in,
// small enough that every fake manifest takes several.
const fakeMessageSize = 16

func (f fakeContent) Read(_ context.Context, req *contentapi.ReadContentRequest, _ ...grpc.CallOption) (contentapi.Content_ReadClient, error) {
	blob, ok := f.blobs[digest.Digest(req.Digest)]
	if !ok {
		return &fakeReadStream{err: status.Errorf(codes.NotFound, "content %s", req.Digest)}, nil
	}
	return &fakeReadStream{data: blob}, nil
}

// fakeReadStream is the stream of one fake read.
type fakeReadStream struct {
	grpc.ClientStream
	data []byte
	err  error
}

func (s *fakeReadStream) Recv() (*contentapi.ReadContentResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.data) == 0 {
		return nil, io.EOF
	}
	n := min(len(s.data), fakeMessageSize)
	resp := &contentapi.ReadContentResponse{Data: s.data[:n]}
	s.data = s.data[n:]
	return resp, nil
}

// layerSize is the size the fake manifests declare for every layer blob.
const layerSize = 42

// layerBlob is the digest the fake manifests give the compressed blob of the
// layer with the given diff ID.
func layerBlob(diffID digest.Digest) digest.Digest {
	return digest.FromString("blob:" + diffID.String())
}

// applyStaged hands the differ the layer with the given diff ID for the
// staging snapshot under key, the way the unpacker does between Prepare and
// Commit.
func applyStaged(ctx context.Context, sn *snapshotter, key string, diffID digest.Digest) error {
	_, err := sn.Apply(ctx, ocispec.Descriptor{Digest: layerBlob(diffID)}, stagingMount(key))
	return err
}

// stageImage puts the manifest and config of an image with the given layer
// diff IDs in the snapshotter's content store, alongside whatever it already
// holds, and returns the manifest digest the runtime would label a pull with
// along with the chain ID of each layer.
func stageImage(t *testing.T, sn *snapshotter, diffIDs ...digest.Digest) (digest.Digest, []digest.Digest) {
	t.Helper()
	return stageImageBy(t, sn, "", diffIDs...)
}

// stageImageBy is stageImage for an image whose config names an author, so
// two images built from the same layers get different manifests.
func stageImageBy(t *testing.T, sn *snapshotter, author string, diffIDs ...digest.Digest) (digest.Digest, []digest.Digest) {
	t.Helper()
	return stageImageConfig(t, sn, ocispec.Image{Author: author, Platform: hostImagePlatform()}, diffIDs...)
}

// hostImagePlatform is the platform the fake images declare, which is the one
// the snapshotter under test runs on.
func hostImagePlatform() ocispec.Platform {
	return ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
}

// loadImage puts an image with the given layer diff IDs in the snapshotter's
// content store the way a runtime's pull or load leaves it — manifest,
// config and every layer blob — and records it under name in the images
// service. It returns what stageImage does.
func loadImage(t *testing.T, sn *snapshotter, name string, diffIDs ...digest.Digest) (digest.Digest, []digest.Digest) {
	t.Helper()
	return loadImageBy(t, sn, name, "", diffIDs...)
}

// loadImageBy is loadImage for an image whose config names an author.
func loadImageBy(t *testing.T, sn *snapshotter, name, author string, diffIDs ...digest.Digest) (digest.Digest, []digest.Digest) {
	t.Helper()
	root, chains := stageImageBy(t, sn, author, diffIDs...)
	loadLayers(sn, diffIDs...)
	imagesOf(sn).add(name, ocispec.MediaTypeImageManifest, root)
	return root, chains
}

// loadLayers puts the layer blobs of the given diff IDs in the snapshotter's
// content store; their bytes are never read, only their presence is checked.
func loadLayers(sn *snapshotter, diffIDs ...digest.Digest) {
	store := contentOf(sn)
	for _, diffID := range diffIDs {
		store.blobs[layerBlob(diffID)] = []byte("layer " + diffID.String())
	}
}

// stageImageConfig is stageImageBy for an image with the given config, whose
// rootfs the layer diff IDs become.
func stageImageConfig(t *testing.T, sn *snapshotter, config ocispec.Image, diffIDs ...digest.Digest) (digest.Digest, []digest.Digest) {
	t.Helper()
	config.RootFS = ocispec.RootFS{Type: "layers", DiffIDs: diffIDs}
	configBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configDigest := digest.FromBytes(configBytes)
	layers := make([]ocispec.Descriptor, len(diffIDs))
	for i, diffID := range diffIDs {
		layers[i] = ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: layerBlob(diffID), Size: layerSize}
	}
	manifest, err := json.Marshal(ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
		Layers: layers,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	root := digest.FromBytes(manifest)
	store := contentOf(sn)
	store.blobs[root] = manifest
	store.blobs[configDigest] = configBytes
	chains := make([]digest.Digest, len(diffIDs))
	copy(chains, diffIDs)
	identity.ChainIDs(chains)
	return root, chains
}

// stagedLabels are the labels the runtime attaches to a pull of an image the
// ocicfs store does not hold, which Prepare answers with a staging snapshot.
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
// images the ocicfs store does not hold.
func newStagingSnapshotter(t *testing.T, engine *fakeEngine) *snapshotter {
	t.Helper()
	return newConfiguredSnapshotter(t, Config{Root: t.TempDir(), AdmitAny: true}, engine, notMounted)
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
	engine.mountChainFunc = func(_ context.Context, chainID, target string, opts ...ocicfs.MountOption) error {
		var mountOpts ocicfs.MountOptions
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

// isStagingMount reports whether mounts is exactly the staging placeholder of
// the snapshot under key.
func isStagingMount(mounts []mount.Mount, key string) bool {
	return len(mounts) == 1 && mounts[0].Type == stagingMountType && mounts[0].Source == key &&
		mounts[0].Target == "" && len(mounts[0].Options) == 0
}

func notMounted(string) (bool, error)    { return false, nil }
func alwaysMounted(string) (bool, error) { return true, nil }

func testImageInfo(root string, chains ...digest.Digest) *ocicfs.ImageInfo {
	layers := make([]ocicfs.LayerInfo, len(chains))
	for i, chainID := range chains {
		layers[i].ChainID = chainID
	}
	return &ocicfs.ImageInfo{Root: digest.FromString(root), Layers: layers}
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

// TestNewSnapshotterReleasesStrayHolds covers a daemon starting over a store
// that a previous run left holds in: the holder a committed leaf names and
// the one a staging leaf names are kept, this snapshotter's holders that no
// record names are released, and holders that are not its own are left alone.
func TestNewSnapshotterReleasesStrayHolds(t *testing.T) {
	ctx := pullNamespace()
	root := t.TempDir()
	engine := fakeEngineForImages()
	engine.objectsDir = t.TempDir()
	first := newConfiguredSnapshotter(t, Config{Root: root, AdmitAny: true}, engine, notMounted)
	layers := []digest.Digest{digest.FromString("layer-1")}
	image, chains := stageImage(t, first, layers...)
	other, otherChains := stageImageBy(t, first, "other", layers...)

	if _, err := first.Prepare(ctx, "moby/1/extract", "", snapshots.WithLabels(stagedLabels("app:v1", image, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, first, "moby/1/extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	committed := "moby/1/" + chains[0].String()
	if err := first.Commit(ctx, committed, "moby/1/extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	// A second image applied but never committed: its staging record names
	// the holder its build took.
	if _, err := first.Prepare(ctx, "moby/2/extract", "", snapshots.WithLabels(stagedLabels("other:v1", other, otherChains[0]))); err != nil {
		t.Fatalf("Prepare(other) error = %v", err)
	}
	if err := applyStaged(ctx, first, "moby/2/extract", layers[0]); err != nil {
		t.Fatalf("Apply(other) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	owned := buildHolder("moby", "moby/1/extract")
	staged := buildHolder("moby", "moby/2/extract")
	stray := buildHolder("moby", "moby/9/extract")
	strayRecord := recordHolder("moby/9/" + chains[0].String())
	engine.held = map[digest.Digest][]string{
		image: {"agent", owned, stray},
		other: {staged, strayRecord},
	}
	second := newConfiguredSnapshotter(t, Config{Root: root, AdmitAny: true}, engine, notMounted)
	awaitGCs(t, engine, 1)
	if _, err := second.Stat(ctx, committed); err != nil {
		t.Fatalf("Stat(%q) error = %v", committed, err)
	}
	engine.mu.Lock()
	releases := slices.Clone(engine.releases)
	engine.mu.Unlock()
	slices.SortFunc(releases, func(a, b holdCall) int { return strings.Compare(a.ref+a.holder, b.ref+b.holder) })
	want := []holdCall{{ref: image.String(), holder: stray}, {ref: other.String(), holder: strayRecord}}
	slices.SortFunc(want, func(a, b holdCall) int { return strings.Compare(a.ref+a.holder, b.ref+b.holder) })
	if !slices.Equal(releases, want) {
		t.Fatalf("engine Release calls = %+v, want the stray holders alone: %+v", releases, want)
	}
}

// TestCleanupCollectsAfterRemove covers the runtime dropping a staged image:
// the record's removal releases the hold and marks the store to be
// collected, and the cleanup containerd runs at the end of that cycle
// collects it. A cleanup with nothing dropped since collects nothing.
func TestCleanupCollectsAfterRemove(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	if err := sn.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if got := engine.gcCount(); got != 1 {
		t.Fatalf("collections after an idle cleanup = %d, want the startup one alone", got)
	}

	layers := []digest.Digest{digest.FromString("layer-1")}
	root, chains := stageImage(t, sn, layers...)
	name := "moby/1/" + chains[0].String()
	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := sn.Commit(ctx, name, "extract"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	// A finished build requests no collection of its own.
	waitClean(t, sn)
	if got := engine.gcCount(); got != 1 {
		t.Fatalf("collections after a finished build = %d, want the startup one alone", got)
	}
	if err := sn.Remove(ctx, name); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if got := engine.gcCount(); got != 1 {
		t.Fatalf("collections after a remove = %d, want none until cleanup", got)
	}
	if err := sn.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if got := engine.gcCount(); got != 2 {
		t.Fatalf("collections after cleanup = %d, want 2", got)
	}
	if err := sn.Cleanup(ctx); err != nil {
		t.Fatalf("second Cleanup() error = %v", err)
	}
	if got := engine.gcCount(); got != 2 {
		t.Fatalf("collections after a second cleanup = %d, want still 2", got)
	}
}

// TestApplyCollectsOnlyWhenBuildFails covers which builds request a
// collection: a finished build leaves the files its image does not
// reference to the next collection, whoever asks for it, while a build that
// failed or was abandoned has no image to reference what it wrote and
// requests one itself.
func TestApplyCollectsOnlyWhenBuildFails(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	layers := []digest.Digest{digest.FromString("layer-1")}
	root, chains := stageImage(t, sn, layers...)

	if _, err := sn.Prepare(ctx, "extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract", layers[0]); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	waitClean(t, sn)
	if got := engine.gcCount(); got != 1 {
		t.Fatalf("collections after a finished build = %d, want the startup one alone", got)
	}

	other, otherChains := stageImageBy(t, sn, "other", digest.FromString("o-1"), digest.FromString("o-2"))
	engine.ingestErr = errors.New("layer failed")
	if _, err := sn.Prepare(ctx, "extract-other", "", snapshots.WithLabels(stagedLabels("other:v1", other, otherChains[0]))); err != nil {
		t.Fatalf("Prepare(other) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract-other", digest.FromString("o-1")); err == nil {
		t.Fatal("Apply(other) succeeded, want the layer failure")
	}
	awaitGCs(t, engine, 2)
	waitClean(t, sn)
	if got := engine.gcCount(); got != 2 {
		t.Fatalf("collections after a failed ingest = %d, want 2", got)
	}

	// A one-layer image's only layer is the leaf, so its apply finishes
	// the build.
	third, thirdChains := stageImageBy(t, sn, "third", digest.FromString("t-1"))
	engine.ingestErr = nil
	engine.finishErr = errors.New("finish failed")
	if _, err := sn.Prepare(ctx, "extract-third", "", snapshots.WithLabels(stagedLabels("third:v1", third, thirdChains[0]))); err != nil {
		t.Fatalf("Prepare(third) error = %v", err)
	}
	if err := applyStaged(ctx, sn, "extract-third", digest.FromString("t-1")); err == nil {
		t.Fatal("Apply(third) succeeded, want the finish failure")
	}
	awaitGCs(t, engine, 3)
	waitClean(t, sn)
	if got := engine.gcCount(); got != 3 {
		t.Fatalf("collections after a failed finish = %d, want 3", got)
	}
}

// TestCleanupRetriesWhenStoreBusy covers a collection finding the store busy
// with a pull: it is not an error, and the snapshotter collects again on its
// own once the interval passes, until a collection goes through.
func TestCleanupRetriesWhenStoreBusy(t *testing.T) {
	interval := gcRetryInterval
	gcRetryInterval = 5 * time.Millisecond
	t.Cleanup(func() { gcRetryInterval = interval })
	engine := fakeEngineForImages()
	engine.gcErr = fmt.Errorf("busy: %w", ocicfs.ErrStoreBusy)
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 2)
	if err := sn.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() on a busy store error = %v, want none", err)
	}
	engine.setGCErr(nil)
	awaitGCs(t, engine, 3)
	deadline := time.Now().Add(time.Second)
	for sn.dirty.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the store stayed marked dirty after a collection went through")
		}
		time.Sleep(time.Millisecond)
	}
	settled := engine.gcCount()
	time.Sleep(20 * gcRetryInterval)
	if got := engine.gcCount(); got != settled {
		t.Fatalf("collections kept running after one succeeded: %d, then %d", settled, got)
	}
}

// TestCleanupReportsOtherFailuresOnce covers a collection failing for a
// reason other than a busy store: the failure is reported and not retried,
// since nothing would change between attempts.
func TestCleanupReportsOtherFailuresOnce(t *testing.T) {
	interval := gcRetryInterval
	gcRetryInterval = 5 * time.Millisecond
	t.Cleanup(func() { gcRetryInterval = interval })
	engine := fakeEngineForImages()
	broken := errors.New("held image has no binding")
	engine.gcErr = broken
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	time.Sleep(20 * gcRetryInterval)
	if got := engine.gcCount(); got != 1 {
		t.Fatalf("collections after a broken store = %d, want the one that failed", got)
	}
	if err := sn.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() with nothing to collect error = %v", err)
	}
	name := commitStaged(t, sn, "app", digest.FromString("layer-1"))
	waitClean(t, sn)
	if err := sn.Remove(pullNamespace(), name); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := sn.Cleanup(context.Background()); !errors.Is(err, broken) {
		t.Fatalf("Cleanup() on a broken store error = %v, want %v", err, broken)
	}
	if err := sn.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup() after the failure was reported error = %v, want none", err)
	}
	if got := engine.gcCount(); got != 2 {
		t.Fatalf("collections = %d, want 2: a reported failure is not retried", got)
	}
}

// commitStaged pulls a one-layer image through the staging path, commits its
// leaf under a runtime name and returns that name.
func commitStaged(t *testing.T, sn *snapshotter, author string, layer digest.Digest) string {
	t.Helper()
	ctx := pullNamespace()
	root, chains := stageImageBy(t, sn, author, layer)
	key := "extract-" + author
	if _, err := sn.Prepare(ctx, key, "", snapshots.WithLabels(stagedLabels(author+":v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := applyStaged(ctx, sn, key, layer); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	name := "moby/1/" + chains[0].String()
	if err := sn.Commit(ctx, name, key); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	return name
}

// cleanupAsync runs Cleanup on its own goroutine and returns its result.
func cleanupAsync(sn *snapshotter) <-chan error {
	done := make(chan error, 1)
	go func() { done <- sn.Cleanup(context.Background()) }()
	return done
}

// TestCleanupSkipsWhenNothingReleased covers a cycle that released no hold,
// such as a container's removal, arriving while a collection runs for
// another reason: its cleanup returns at once instead of waiting for it.
func TestCleanupSkipsWhenNothingReleased(t *testing.T) {
	engine := fakeEngineForImages()
	gate := make(chan struct{})
	engine.gcGate = gate
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	select {
	case err := <-cleanupAsync(sn):
		if err != nil {
			t.Fatalf("Cleanup() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cleanup() waited for a collection that covers no release of its cycle")
	}
	close(gate)
}

// TestCleanupWaitsForCoveringCollection covers an image's removal while the
// snapshotter is already collecting: the cleanup waits for that collection
// when it covers the release, and runs one of its own when the running one
// began before the release, so the store is reclaimed when it returns.
func TestCleanupWaitsForCoveringCollection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		releaseFirst bool
		wantGCs      int
	}{
		{name: "running collection covers the release", releaseFirst: true, wantGCs: 2},
		{name: "running collection began before the release", releaseFirst: false, wantGCs: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := fakeEngineForImages()
			sn := newStagingSnapshotter(t, engine)
			awaitGCs(t, engine, 1)
			name := commitStaged(t, sn, "app", digest.FromString("layer-1"))
			waitClean(t, sn)

			gate := make(chan struct{})
			engine.setGCGate(gate)
			remove := func() {
				if err := sn.Remove(pullNamespace(), name); err != nil {
					t.Fatalf("Remove() error = %v", err)
				}
			}
			if tc.releaseFirst {
				remove()
			}
			sn.collectSoon()
			awaitGCs(t, engine, 2)
			if !tc.releaseFirst {
				remove()
			}

			// What the collections had covered when Cleanup returned: a
			// Cleanup that returned before the release was collected shows
			// it, whatever the scheduling.
			coveredAtReturn := make(chan uint64, 1)
			go func() {
				if err := sn.Cleanup(context.Background()); err != nil {
					t.Errorf("Cleanup() error = %v", err)
				}
				coveredAtReturn <- sn.covered.Load()
			}()
			close(gate)
			select {
			case covered := <-coveredAtReturn:
				if released := sn.releases.Load(); covered != released {
					t.Fatalf("Cleanup() returned with %d releases collected, want all %d", covered, released)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Cleanup() did not return after the collection finished")
			}
			if got := engine.gcCount(); got != tc.wantGCs {
				t.Fatalf("collections = %d, want %d", got, tc.wantGCs)
			}
		})
	}
}

// waitClean waits until no collection is pending or running.
func waitClean(t *testing.T, sn *snapshotter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sn.reclaimMu.Lock()
		dirty := sn.dirty.Load()
		sn.reclaimMu.Unlock()
		if !dirty {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the store stayed marked dirty")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCloseStopsPendingRetry covers shutting down while a collection is
// waiting to be retried: Close returns without waiting out the interval.
func TestCloseStopsPendingRetry(t *testing.T) {
	interval := gcRetryInterval
	gcRetryInterval = time.Hour
	t.Cleanup(func() { gcRetryInterval = interval })
	engine := fakeEngineForImages()
	engine.gcErr = fmt.Errorf("busy: %w", ocicfs.ErrStoreBusy)
	sn, err := newSnapshotter(context.Background(), Config{Root: t.TempDir(), AdmitAny: true}, engine)
	if err != nil {
		t.Fatalf("newSnapshotter() error = %v", err)
	}
	awaitGCs(t, engine, 1)
	done := make(chan error, 1)
	go func() { done <- sn.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() waited on the retry interval")
	}
}

// TestRemoveWaitsForFinishingBuild covers containerd unwinding a pull while
// its last layer's apply is finishing the image: Remove of the staging leaf
// record closes that build first — waiting for the finish, which takes the
// hold as it completes — and only then releases the record's holder, so a
// hold that lands during the removal is not left with nothing to release it.
func TestRemoveWaitsForFinishingBuild(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	engine.finishGate = make(chan struct{})
	engine.finishDone = make(chan struct{})
	sn := newStagingSnapshotter(t, engine)
	layers := []digest.Digest{digest.FromString("layer-1")}
	root, chains := stageImage(t, sn, layers...)

	if _, err := sn.Prepare(ctx, "moby/1/extract", "", snapshots.WithLabels(stagedLabels("app:v1", root, chains[0]))); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	applied := make(chan error, 1)
	go func() { applied <- applyStaged(ctx, sn, "moby/1/extract", layers[0]) }()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(engine.callLog(), "build0.finish") {
		if time.Now().After(deadline) {
			t.Fatal("the apply never reached Finish")
		}
		time.Sleep(time.Millisecond)
	}

	if err := sn.Remove(context.Background(), "moby/1/extract"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := <-applied; err != nil {
		t.Logf("Apply() after the removal returned %v", err)
	}
	holder := buildHolder("moby", "moby/1/extract")
	want := []holdCall{{ref: root.String(), holder: holder}}
	engine.mu.Lock()
	holds, releases := slices.Clone(engine.holds), slices.Clone(engine.releases)
	engine.mu.Unlock()
	if !slices.Equal(holds, want) {
		t.Fatalf("engine Hold calls = %+v, want the finishing build's hold %+v", holds, want)
	}
	if !slices.Equal(releases, want) {
		t.Fatalf("engine Release calls = %+v, want the hold released after the finish", releases)
	}
	if calls := engine.callLog(); slices.Index(calls, "build0.close") < slices.Index(calls, "build0.finish") {
		t.Fatalf("engine calls = %v, want the close after the finish began", calls)
	}
	engine.mu.Lock()
	events := slices.Clone(engine.events)
	engine.mu.Unlock()
	if want := []string{"hold:" + holder, "release:" + holder}; !slices.Equal(events, want) {
		t.Fatalf("hold/release order = %v, want %v: the release must follow the finish's hold", events, want)
	}
	if len(sn.finishing) != 0 {
		t.Fatalf("finishing builds = %d after the removal, want none", len(sn.finishing))
	}
}

// pullImage drives the labeled unpack handshake of an image through the
// snapshotter — Prepare, Apply and Commit per layer, under keys and names
// shaped as containerd files them, with prefix telling one pull's records
// from another's — and returns the committed name of each layer's record.
// Commit is handed the handshake's labels again, as containerd passes a
// backend the snapshot.ref and containerd.io/snapshot/* labels it was given.
func pullImage(t *testing.T, sn *snapshotter, prefix, imageRef string, root digest.Digest, chains, layers []digest.Digest) []string {
	t.Helper()
	ctx := pullNamespace()
	names := make([]string, len(chains))
	parent := ""
	for i, chain := range chains {
		key := fmt.Sprintf("%s/%d/extract", prefix, i)
		labels := snapshots.WithLabels(stagedLabels(imageRef, root, chain))
		if _, err := sn.Prepare(ctx, key, parent, labels); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if err := applyStaged(ctx, sn, key, layers[i]); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
		names[i] = fmt.Sprintf("%s/%d/%s", prefix, i, chain)
		if err := sn.Commit(ctx, names[i], key, labels); err != nil {
			t.Fatalf("Commit(%s) error = %v", names[i], err)
		}
		parent = names[i]
	}
	return names
}

// derivedPull is what a promotion test starts from: the image B = (x, y, w)
// pulled through the labeled path, which leaves the chain (x, y) as a
// committed staged record that is no chain member. A pull of the base image
// A = (x, y) then finds that record present, skips every layer, and leaves
// A unbuilt: the state a container over A meets.
type derivedPull struct {
	// x, y and w are the layers' diff IDs.
	x, y, w digest.Digest
	// rootB is B's manifest digest; base and leaf are the committed names of
	// the (x, y) record and of B's last record; baseChain is the chain the
	// base record stands for; leafHolder is the holder B's build took.
	rootB      digest.Digest
	base, leaf string
	baseChain  digest.Digest
	leafHolder string
	// baseLabels are the labels the pull left on the base record.
	baseLabels map[string]string
}

func pullDerived(t *testing.T, sn *snapshotter) derivedPull {
	t.Helper()
	d := derivedPull{x: digest.FromString("layer-x"), y: digest.FromString("layer-y"), w: digest.FromString("layer-w")}
	var chains []digest.Digest
	d.rootB, chains = stageImageBy(t, sn, "derived", d.x, d.y, d.w)
	names := pullImage(t, sn, "moby", "registry.example/b:v1", d.rootB, chains, []digest.Digest{d.x, d.y, d.w})
	d.base, d.leaf, d.baseChain = names[1], names[2], chains[1]
	// The build's holder is named after the staging record that started it.
	d.leafHolder = buildHolder("moby", "moby/0/extract")
	info, err := sn.Stat(pullNamespace(), d.base)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", d.base, err)
	}
	if info.Kind != snapshots.KindCommitted || info.Labels[stagingLabel] == "" || info.Labels[chainMemberLabel] != "" {
		t.Fatalf("base record = %+v, want a committed staged record that is no chain member", info)
	}
	d.baseLabels = info.Labels
	return d
}

// assertUnpromoted checks that the base record of a derived pull is exactly
// as the pull left it.
func assertUnpromoted(t *testing.T, sn *snapshotter, d derivedPull) {
	t.Helper()
	info, err := sn.Stat(pullNamespace(), d.base)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", d.base, err)
	}
	if !maps.Equal(info.Labels, d.baseLabels) {
		t.Fatalf("base record labels = %v, want unchanged %v", info.Labels, d.baseLabels)
	}
}

// awaitCall waits for the engine's call log to contain call.
func awaitCall(t *testing.T, engine *fakeEngine, call string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(engine.callLog(), call) {
		if time.Now().After(deadline) {
			t.Fatalf("engine calls = %v, want %s", engine.callLog(), call)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPreparePromotesRecordedChain covers a container over a base image
// whose pull built nothing: a derived image pulled first left the base's
// chain as a staged record, so the base's own pull found it and skipped its
// layers. The container's Prepare promotes the record — the base is built
// whole from the runtime's content store and held under the record's own
// holder, the record becomes a chain member naming the base rather than the
// derived image it was staged for — and mounts it. The base then outlives
// the derived image's removal and goes with its own record.
func TestPreparePromotesRecordedChain(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	d := pullDerived(t, sn)
	rootA, _ := loadImage(t, sn, "registry.example/a:v1", d.x, d.y)
	imagesOf(sn).add("registry.example/b:v1", ocispec.MediaTypeImageManifest, d.rootB)
	var calls []mountCall
	recordMounts(engine, &calls)

	mounts, err := sn.Prepare(ctx, "moby/9/container-init", d.base)
	if err != nil {
		t.Fatalf("Prepare(container-init) error = %v", err)
	}
	if len(calls) != 1 || calls[0].chainID != d.baseChain.String() {
		t.Fatalf("MountChain calls = %+v, want the base chain %s", calls, d.baseChain)
	}
	if got := optionValue(t, mounts[0], "lowerdir"); got != calls[0].target+"::"+engine.objectsDir {
		t.Fatalf("lowerdir = %q, want the promoted base image only", got)
	}
	if len(engine.builds) != 2 {
		t.Fatalf("engine NewBuild calls = %+v, want the derived pull's and the promotion's", engine.builds)
	}
	holder := recordHolder(d.base)
	build := engine.builds[1]
	if build.ref != "registry.example/a:v1" || build.opts.ResolvedRoot != rootA || build.opts.Hold != holder ||
		build.opts.BlobSource != ocicfs.BlobSource(sn.blobs) {
		t.Fatalf("promotion NewBuild(%q, %+v), want the base image rooted at %s held under %s from the content store", build.ref, build.opts, rootA, holder)
	}
	if calls := engine.callLog(); calls[len(calls)-1] != "build1.finish" {
		t.Fatalf("engine calls = %v, want the promotion's build finished whole", calls)
	}
	wantHolds := []holdCall{{ref: d.rootB.String(), holder: d.leafHolder}, {ref: rootA.String(), holder: holder}}
	if !slices.Equal(engine.holds, wantHolds) {
		t.Fatalf("engine Hold calls = %+v, want %+v", engine.holds, wantHolds)
	}
	if len(sn.builds) != 0 || len(sn.finishing) != 0 {
		t.Fatalf("open builds = %d, finishing = %d after the promotion, want none", len(sn.builds), len(sn.finishing))
	}
	info, err := sn.Stat(ctx, d.base)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", d.base, err)
	}
	want := map[string]string{
		intermediateSnapshotLabel: d.baseChain.String(),
		chainMemberLabel:          "true",
		rootDigestLabel:           rootA.String(),
		holderLabel:               holder,
		imageRefLabel:             "registry.example/a:v1",
		manifestDigestLabel:       rootA.String(),
	}
	if !maps.Equal(info.Labels, want) {
		t.Fatalf("promoted record labels = %v, want %v", info.Labels, want)
	}

	// The derived image goes first, as containerd removes leaf-first: only
	// its own hold is released. The promoted record releases the base's.
	if err := sn.Remove(ctx, d.leaf); err != nil {
		t.Fatalf("Remove(%s) error = %v", d.leaf, err)
	}
	if want := wantHolds[:1]; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls = %+v after the derived leaf's removal, want %+v", engine.releases, want)
	}
	if err := sn.Remove(ctx, "moby/9/container-init"); err != nil {
		t.Fatalf("Remove(container-init) error = %v", err)
	}
	if err := sn.Remove(ctx, d.base); err != nil {
		t.Fatalf("Remove(%s) error = %v", d.base, err)
	}
	if !slices.Equal(engine.releases, wantHolds) {
		t.Fatalf("engine Release calls = %+v after the promoted record's removal, want %+v", engine.releases, wantHolds)
	}
}

// TestPreparePromotionOutOfBand covers the promotion on a daemon that builds
// nothing from runtime content: the record is declined with a message naming
// the recovery, the runtime's records are not even consulted, and nothing
// changes.
func TestPreparePromotionOutOfBand(t *testing.T) {
	ctx := pullNamespace()
	root := t.TempDir()
	engine := fakeEngineForImages()
	engine.objectsDir = t.TempDir()
	staged := newConfiguredSnapshotter(t, Config{Root: root, AdmitAny: true}, engine, notMounted)
	d := pullDerived(t, staged)
	loadImage(t, staged, "registry.example/a:v1", d.x, d.y)
	if err := staged.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sn := newConfiguredSnapshotter(t, Config{Root: root}, engine, notMounted)
	sn.blobs.client, sn.blobs.images = staged.blobs.client, staged.blobs.images
	_, err := sn.Prepare(ctx, "moby/9/container-init", d.base)
	if !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), "use ocicfs pull") {
		t.Fatalf("Prepare(container-init) error = %v, want failed precondition naming ocicfs", err)
	}
	if len(engine.builds) != 1 {
		t.Fatalf("engine NewBuild calls = %+v, want the derived pull's alone", engine.builds)
	}
	if n := imagesOf(sn).listCount(); n != 0 {
		t.Fatalf("images listed %d times, want none: the decline consults no record", n)
	}
	assertUnpromoted(t, sn, d)
	if _, err := sn.Stat(ctx, "moby/9/container-init"); !errdefs.IsNotFound(err) {
		t.Fatalf("Stat(container-init) error = %v, want not found", err)
	}
}

// TestPreparePromotionFailsClosed covers a promotion with nothing to promote
// from — no image ends in the chain, or the one that does lacks a layer blob
// — and one whose build cannot begin or fails: the Prepare fails, the record
// is left as it was, a collection is asked for, and a hold the failed build
// had already written is released, so a retry that picks another image
// leaves exactly one root held under the record's holder.
func TestPreparePromotionFailsClosed(t *testing.T) {
	ctx := pullNamespace()
	t.Run("no image ends in the chain", func(t *testing.T) {
		engine := fakeEngineForImages()
		sn := newStagingSnapshotter(t, engine)
		d := pullDerived(t, sn)
		imagesOf(sn).add("registry.example/b:v1", ocispec.MediaTypeImageManifest, d.rootB)
		loadLayers(sn, d.x, d.y, d.w)
		if _, err := sn.Prepare(ctx, "moby/9/container-init", d.base); !errdefs.IsFailedPrecondition(err) {
			t.Fatalf("Prepare(container-init) error = %v, want failed precondition", err)
		}
		if len(engine.builds) != 1 || len(engine.holds) != 1 {
			t.Fatalf("engine builds = %+v, holds = %+v, want the derived pull's alone", engine.builds, engine.holds)
		}
		assertUnpromoted(t, sn, d)
		if _, err := sn.Stat(ctx, "moby/9/container-init"); !errdefs.IsNotFound(err) {
			t.Fatalf("Stat(container-init) error = %v, want not found", err)
		}
	})
	t.Run("the image lacks a layer blob", func(t *testing.T) {
		engine := fakeEngineForImages()
		sn := newStagingSnapshotter(t, engine)
		d := pullDerived(t, sn)
		rootA, _ := stageImage(t, sn, d.x, d.y)
		imagesOf(sn).add("registry.example/a:v1", ocispec.MediaTypeImageManifest, rootA)
		loadLayers(sn, d.y)
		if _, err := sn.Prepare(ctx, "moby/9/container-init", d.base); !errdefs.IsFailedPrecondition(err) {
			t.Fatalf("Prepare(container-init) error = %v, want failed precondition", err)
		}
		if len(engine.builds) != 1 || len(engine.holds) != 1 {
			t.Fatalf("engine builds = %+v, holds = %+v, want the derived pull's alone", engine.builds, engine.holds)
		}
		assertUnpromoted(t, sn, d)
	})
	t.Run("the build cannot begin", func(t *testing.T) {
		engine := fakeEngineForImages()
		sn := newStagingSnapshotter(t, engine)
		awaitGCs(t, engine, 1)
		d := pullDerived(t, sn)
		loadImage(t, sn, "registry.example/a:v1", d.x, d.y)
		engine.newBuildErr = errors.New("begin failed")
		if _, err := sn.Prepare(ctx, "moby/9/container-init", d.base); err == nil || !strings.Contains(err.Error(), "begin failed") {
			t.Fatalf("Prepare(container-init) error = %v, want the build failure", err)
		}
		if len(engine.builds) != 1 || len(engine.holds) != 1 || len(engine.releases) != 0 {
			t.Fatalf("engine builds = %+v, holds = %+v, releases = %+v, want the derived pull's alone", engine.builds, engine.holds, engine.releases)
		}
		awaitGCs(t, engine, 2)
		assertUnpromoted(t, sn, d)
		if _, err := sn.Stat(ctx, "moby/9/container-init"); !errdefs.IsNotFound(err) {
			t.Fatalf("Stat(container-init) error = %v, want not found", err)
		}
	})
	t.Run("the build fails", func(t *testing.T) {
		engine := fakeEngineForImages()
		sn := newStagingSnapshotter(t, engine)
		awaitGCs(t, engine, 1)
		d := pullDerived(t, sn)
		rootA, _ := loadImage(t, sn, "registry.example/a:v1", d.x, d.y)
		engine.finishErr = errors.New("finish failed")
		if _, err := sn.Prepare(ctx, "moby/9/container-init", d.base); err == nil || !strings.Contains(err.Error(), "finish failed") {
			t.Fatalf("Prepare(container-init) error = %v, want the build failure", err)
		}
		holder := recordHolder(d.base)
		if len(engine.builds) != 2 || len(engine.holds) != 1 {
			t.Fatalf("engine builds = %+v, holds = %+v, want one failed promotion build and no hold for it", engine.builds, engine.holds)
		}
		if want := []holdCall{{ref: rootA.String(), holder: holder}}; !slices.Equal(engine.releases, want) || engine.releaseCtxErrs[0] != nil {
			t.Fatalf("engine Release calls = %+v (ctx errors %v), want the failed build's holder released %+v on a live context", engine.releases, engine.releaseCtxErrs, want)
		}
		awaitGCs(t, engine, 2)
		assertUnpromoted(t, sn, d)
		if _, err := sn.Stat(ctx, "moby/9/container-init"); !errdefs.IsNotFound(err) {
			t.Fatalf("Stat(container-init) error = %v, want not found", err)
		}
	})
	t.Run("the build fails with its hold written", func(t *testing.T) {
		engine := fakeEngineForImages()
		sn := newStagingSnapshotter(t, engine)
		awaitGCs(t, engine, 1)
		d := pullDerived(t, sn)
		rootM1, _ := stageImageBy(t, sn, "m1", d.x, d.y)
		rootM2, _ := stageImageBy(t, sn, "m2", d.x, d.y)
		if rootM2 > rootM1 {
			rootM1, rootM2 = rootM2, rootM1
		}
		loadLayers(sn, d.x, d.y)
		imagesOf(sn).add("registry.example/a:m1", ocispec.MediaTypeImageManifest, rootM1)
		engine.finishHeldErr = errors.New("hold not synced")
		if _, err := sn.Prepare(ctx, "moby/9/first-init", d.base); err == nil || !strings.Contains(err.Error(), "hold not synced") {
			t.Fatalf("Prepare(first-init) error = %v, want the build failure", err)
		}
		holder := recordHolder(d.base)
		if want := []holdCall{{ref: rootM1.String(), holder: holder}}; !slices.Equal(engine.releases, want) {
			t.Fatalf("engine Release calls = %+v, want the failed build's hold released %+v", engine.releases, want)
		}
		awaitGCs(t, engine, 2)
		assertUnpromoted(t, sn, d)

		engine.finishHeldErr = nil
		imagesOf(sn).add("registry.example/a:m2", ocispec.MediaTypeImageManifest, rootM2)
		recordMounts(engine, &[]mountCall{})
		if _, err := sn.Prepare(ctx, "moby/9/second-init", d.base); err != nil {
			t.Fatalf("Prepare(second-init) error = %v", err)
		}
		info, err := sn.Stat(ctx, d.base)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", d.base, err)
		}
		if info.Labels[rootDigestLabel] != rootM2.String() {
			t.Fatalf("promoted record labels = %v, want the retry's root %s", info.Labels, rootM2)
		}
		wantHolds := []holdCall{{ref: d.rootB.String(), holder: d.leafHolder}, {ref: rootM1.String(), holder: holder}, {ref: rootM2.String(), holder: holder}}
		if !slices.Equal(engine.holds, wantHolds) || len(engine.releases) != 1 {
			t.Fatalf("engine holds = %+v, releases = %+v, want exactly one root left held under %s", engine.holds, engine.releases, holder)
		}
	})
}

// TestPreparePromotionIsSerialized covers a second mount walk reaching the
// record while the first promotes it. The second holds the record as it
// read it before the promotion — unpromoted — and waits for the first; once
// the first has published, the second reads the record again under the
// lock, finds the member and takes its image, although a manifest it would
// have preferred was registered meanwhile. The same stale reading after
// the publication does the same without waiting. A label update the runtime
// makes on the record during the build survives the publish.
func TestPreparePromotionIsSerialized(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	d := pullDerived(t, sn)
	rootM1, _ := stageImageBy(t, sn, "m1", d.x, d.y)
	rootM2, _ := stageImageBy(t, sn, "m2", d.x, d.y)
	if rootM2 > rootM1 {
		rootM1, rootM2 = rootM2, rootM1
	}
	loadLayers(sn, d.x, d.y)
	imagesOf(sn).add("registry.example/a:m1", ocispec.MediaTypeImageManifest, rootM1)
	var mounted []mountCall
	recordMounts(engine, &mounted)
	engine.finishGate = make(chan struct{})
	// The record as a walk reads it before the promotion.
	id, info, err := sn.snapshotInfo(ctx, d.base)
	if err != nil {
		t.Fatalf("snapshotInfo(%s) error = %v", d.base, err)
	}
	stale := ancestor{id: id, name: d.base, labels: info.Labels}

	type composed struct {
		lowers []string
		found  bool
		err    error
	}
	compose := func() <-chan composed {
		done := make(chan composed, 1)
		go func() {
			lowers, found, err := sn.composeLowers(ctx, []ancestor{stale})
			done <- composed{lowers: lowers, found: found, err: err}
		}()
		return done
	}
	first := compose()
	awaitCall(t, engine, "build1.finish")
	// The first holds the promotion lock inside its build, so the second's
	// walk, with the same unpromoted reading of the record, queues behind it.
	if sn.promoteMu.TryLock() {
		sn.promoteMu.Unlock()
		t.Fatal("the promotion lock is free while the first promotion builds")
	}
	second := compose()
	imagesOf(sn).add("registry.example/a:m2", ocispec.MediaTypeImageManifest, rootM2)
	gc := snapshots.Info{Name: d.base, Labels: map[string]string{"containerd.io/gc.root": "keep"}}
	if _, err := sn.Update(ctx, gc, "labels.containerd.io/gc.root"); err != nil {
		t.Fatalf("Update(gc label) error = %v", err)
	}
	engine.finishGateOnce.Do(func() { close(engine.finishGate) })

	fsDir := filepath.Join(sn.getSnapshotDir(id), "fs")
	for _, done := range []<-chan composed{first, second} {
		c := <-done
		if c.err != nil {
			t.Fatalf("composeLowers() error = %v", c.err)
		}
		if !c.found || !slices.Equal(c.lowers, []string{fsDir}) {
			t.Fatalf("composeLowers() = %v, %v, want the promoted record's image mount alone", c.lowers, c.found)
		}
	}
	if len(engine.builds) != 2 || len(engine.holds) != 2 {
		t.Fatalf("engine builds = %+v, holds = %+v, want one promotion build and one hold", engine.builds, engine.holds)
	}
	info, err = sn.Stat(ctx, d.base)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", d.base, err)
	}
	if info.Labels[rootDigestLabel] != rootM1.String() || info.Labels[holderLabel] != recordHolder(d.base) {
		t.Fatalf("promoted record labels = %v, want the first promotion's root %s under the record's holder", info.Labels, rootM1)
	}
	if info.Labels["containerd.io/gc.root"] != "keep" {
		t.Fatalf("promoted record labels = %v, want the runtime's gc label kept", info.Labels)
	}

	// A walk that read the record before the publication and reaches it
	// only now takes the published image too.
	if c := <-compose(); c.err != nil || !slices.Equal(c.lowers, []string{fsDir}) {
		t.Fatalf("composeLowers() after the publication = %v, %v, want the promoted record's image mount", c.lowers, c.err)
	}
	if len(engine.builds) != 2 || len(engine.holds) != 2 || len(mounted) != 3 {
		t.Fatalf("engine builds = %+v, holds = %+v, mounts = %d, want no second build or hold and a mount per walk", engine.builds, engine.holds, len(mounted))
	}
	if _, err := sn.Prepare(ctx, "moby/9/container-init", d.base); err != nil {
		t.Fatalf("Prepare(container-init) over the promoted record error = %v", err)
	}
}

// TestPreparePromotionReleasesOnPublishFailure covers a built promotion whose
// publish fails under a caller that has cancelled meanwhile: the hold the
// build took is released all the same, on a context the cancellation does not
// reach, a collection is asked for, and the record is left as it was — so a
// retry promotes afresh and ends with exactly one root held under the
// record's holder.
func TestPreparePromotionReleasesOnPublishFailure(t *testing.T) {
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	d := pullDerived(t, sn)
	rootM1, _ := stageImageBy(t, sn, "m1", d.x, d.y)
	rootM2, _ := stageImageBy(t, sn, "m2", d.x, d.y)
	if rootM2 > rootM1 {
		rootM1, rootM2 = rootM2, rootM1
	}
	loadLayers(sn, d.x, d.y)
	imagesOf(sn).add("registry.example/a:m1", ocispec.MediaTypeImageManifest, rootM1)
	recordMounts(engine, &[]mountCall{})
	engine.finishGate = make(chan struct{})
	sn.updateInfo = func(context.Context, snapshots.Info, ...string) (snapshots.Info, error) {
		return snapshots.Info{}, errors.New("publish failed")
	}

	ctx, cancel := context.WithCancel(pullNamespace())
	done := make(chan error, 1)
	go func() {
		_, err := sn.Prepare(ctx, "moby/9/first-init", d.base)
		done <- err
	}()
	awaitCall(t, engine, "build1.finish")
	cancel()
	engine.finishGateOnce.Do(func() { close(engine.finishGate) })
	if err := <-done; err == nil || !strings.Contains(err.Error(), "publish failed") {
		t.Fatalf("Prepare(first-init) error = %v, want the publish failure", err)
	}
	holder := recordHolder(d.base)
	if want := []holdCall{{ref: rootM1.String(), holder: holder}}; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine Release calls = %+v, want the promotion's hold released %+v", engine.releases, want)
	}
	if engine.releaseCtxErrs[0] != nil {
		t.Fatalf("the hold was released on a cancelled context: %v", engine.releaseCtxErrs[0])
	}
	awaitGCs(t, engine, 2)
	assertUnpromoted(t, sn, d)

	sn.updateInfo = storage.UpdateInfo
	imagesOf(sn).add("registry.example/a:m2", ocispec.MediaTypeImageManifest, rootM2)
	if _, err := sn.Prepare(pullNamespace(), "moby/9/second-init", d.base); err != nil {
		t.Fatalf("Prepare(second-init) error = %v", err)
	}
	info, err := sn.Stat(pullNamespace(), d.base)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", d.base, err)
	}
	if info.Labels[rootDigestLabel] != rootM2.String() || info.Labels[holderLabel] != holder {
		t.Fatalf("promoted record labels = %v, want the retry's root %s under %s", info.Labels, rootM2, holder)
	}
	wantHolds := []holdCall{{ref: d.rootB.String(), holder: d.leafHolder}, {ref: rootM1.String(), holder: holder}, {ref: rootM2.String(), holder: holder}}
	if !slices.Equal(engine.holds, wantHolds) || len(engine.releases) != 1 {
		t.Fatalf("engine holds = %+v, releases = %+v, want exactly one root left held under %s", engine.holds, engine.releases, holder)
	}
}

// TestPreparePromotionPrefersHostPlatform covers a manifest of another
// platform over the same layers, ordered ahead of the host's: the host's is
// built.
func TestPreparePromotionPrefersHostPlatform(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	d := pullDerived(t, sn)
	rootHost, _ := loadImage(t, sn, "registry.example/a:host", d.x, d.y)
	foreign := ocispec.Image{Platform: ocispec.Platform{OS: "plan9", Architecture: "mips"}}
	var rootForeign digest.Digest
	for i := 0; rootForeign == "" || rootForeign > rootHost; i++ {
		foreign.Author = fmt.Sprintf("foreign-%d", i)
		rootForeign, _ = stageImageConfig(t, sn, foreign, d.x, d.y)
	}
	imagesOf(sn).add("registry.example/a:foreign", ocispec.MediaTypeImageManifest, rootForeign)
	recordMounts(engine, &[]mountCall{})

	if _, err := sn.Prepare(ctx, "moby/9/container-init", d.base); err != nil {
		t.Fatalf("Prepare(container-init) error = %v", err)
	}
	if len(engine.builds) != 2 || engine.builds[1].opts.ResolvedRoot != rootHost {
		t.Fatalf("engine NewBuild calls = %+v, want the host manifest %s built", engine.builds, rootHost)
	}
}

// TestLoadedManifestsWalksImages covers the walk of the runtime's image
// records: a manifest reached directly and through an index collects every
// name that leads to it, an index is descended one level for its image
// manifests, and records, index entries, manifests and configs that are
// absent, describe no layers, disagree on the layer count or belong to
// another platform are passed over.
func TestLoadedManifestsWalksImages(t *testing.T) {
	ctx := pullNamespace()
	sn := newStagingSnapshotter(t, fakeEngineForImages())
	x, y := digest.FromString("layer-x"), digest.FromString("layer-y")
	rootA, chainsA := stageImage(t, sn, x, y)
	rootB, chainsB := stageImageBy(t, sn, "b", x)
	attestation, _ := stageImageConfig(t, sn, ocispec.Image{Platform: hostImagePlatform()})
	rootForeign, _ := stageImageConfig(t, sn, ocispec.Image{Platform: ocispec.Platform{OS: "plan9", Architecture: "mips"}}, x)
	store := contentOf(sn)
	var manifestB ocispec.Manifest
	if err := json.Unmarshal(store.blobs[rootB], &manifestB); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	configless := manifestB
	configless.Config.Digest = digest.FromString("absent config")
	noConfig := putJSON(t, store, configless)
	mismatched := manifestB
	mismatched.Layers = append(mismatched.Layers, mismatched.Layers[0])
	miscounted := putJSON(t, store, mismatched)
	index := putJSON(t, store, ocispec.Index{Manifests: []ocispec.Descriptor{
		{MediaType: ocispec.MediaTypeImageManifest, Digest: rootA},
		{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("absent variant")},
		{MediaType: ocispec.MediaTypeImageManifest, Digest: attestation},
		{MediaType: ocispec.MediaTypeImageIndex, Digest: index2(t, store, rootB)},
	}})
	records := imagesOf(sn)
	records.add("registry.example/a:v1", ocispec.MediaTypeImageIndex, index)
	records.add("moby-dangling@"+rootA.String(), ocispec.MediaTypeImageManifest, rootA)
	records.add("registry.example/b:v1", images.MediaTypeDockerSchema2Manifest, rootB)
	records.add("registry.example/b:v2", ocispec.MediaTypeImageManifest, rootB)
	records.add("registry.example/absent:v1", ocispec.MediaTypeImageManifest, digest.FromString("absent manifest"))
	records.add("registry.example/absent:index", ocispec.MediaTypeImageIndex, digest.FromString("absent index"))
	records.add("registry.example/foreign:v1", ocispec.MediaTypeImageManifest, rootForeign)
	records.add("registry.example/noconfig:v1", ocispec.MediaTypeImageManifest, noConfig)
	records.add("registry.example/miscounted:v1", ocispec.MediaTypeImageManifest, miscounted)
	records.add("registry.example/other:v1", "application/vnd.example.other", rootB)

	manifests, err := sn.loadedManifests(ctx)
	if err != nil {
		t.Fatalf("loadedManifests() error = %v", err)
	}
	want := []loadedManifest{
		{root: rootA, names: []string{"registry.example/a:v1", "moby-dangling@" + rootA.String()}, diffIDs: []digest.Digest{x, y}, chains: chainsA},
		{root: rootB, names: []string{"registry.example/b:v1", "registry.example/b:v2"}, diffIDs: []digest.Digest{x}, chains: chainsB},
	}
	slices.SortFunc(want, func(a, b loadedManifest) int { return strings.Compare(a.root.String(), b.root.String()) })
	if len(manifests) != len(want) {
		t.Fatalf("loadedManifests() = %+v, want %+v", manifests, want)
	}
	for i, m := range manifests {
		if m.root != want[i].root || !slices.Equal(m.names, want[i].names) || !slices.Equal(m.diffIDs, want[i].diffIDs) || !slices.Equal(m.chains, want[i].chains) {
			t.Fatalf("loadedManifests()[%d] = %+v, want %+v", i, m, want[i])
		}
		if len(m.layers) != len(m.chains) || m.layers[0].Digest != layerBlob(x) {
			t.Fatalf("loadedManifests()[%d].layers = %+v, want the manifest's layer descriptors", i, m.layers)
		}
	}
	if !slices.Equal(records.lists, []string{"moby"}) {
		t.Fatalf("images listed in namespaces %v, want once in moby", records.lists)
	}

	records.err = errors.New("images unavailable")
	if _, err := sn.loadedManifests(ctx); err == nil {
		t.Fatal("loadedManifests() succeeded with the images service failing")
	}
}

// putJSON puts v in the fake content store and returns its digest.
func putJSON(t *testing.T, store fakeContent, v any) digest.Digest {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dgst := digest.FromBytes(data)
	store.blobs[dgst] = data
	return dgst
}

// index2 puts a one-entry index naming root in the fake content store and
// returns its digest.
func index2(t *testing.T, store fakeContent, root digest.Digest) digest.Digest {
	t.Helper()
	return putJSON(t, store, ocispec.Index{Manifests: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageManifest, Digest: root}}})
}

// TestBuildableManifestRequiresEveryBlob covers the choice of the manifest
// to build a chain from: only one whose last chain it is, only one whose
// every layer blob is present, and the first in digest order of those.
func TestBuildableManifestRequiresEveryBlob(t *testing.T) {
	ctx := pullNamespace()
	sn := newStagingSnapshotter(t, fakeEngineForImages())
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	rootA1, chains := stageImageBy(t, sn, "a1", x, y)
	rootA2, _ := stageImageBy(t, sn, "a2", x, y)
	rootC, chainsC := stageImageBy(t, sn, "c", x, y, w)
	if rootA2 < rootA1 {
		rootA1, rootA2 = rootA2, rootA1
	}
	// Alt describes the same layers as A1 and A2, with its first one under
	// another compressed blob.
	store := contentOf(sn)
	var alt ocispec.Manifest
	if err := json.Unmarshal(store.blobs[rootA1], &alt); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	altBlob := digest.FromString("alternate blob of layer-x")
	alt.Layers[0].Digest = altBlob
	rootAlt := putJSON(t, store, alt)
	records := imagesOf(sn)
	records.add("a1", ocispec.MediaTypeImageManifest, rootA1)
	records.add("a2", ocispec.MediaTypeImageManifest, rootA2)
	records.add("alt", ocispec.MediaTypeImageManifest, rootAlt)
	records.add("c", ocispec.MediaTypeImageManifest, rootC)
	manifests, err := sn.loadedManifests(ctx)
	if err != nil {
		t.Fatalf("loadedManifests() error = %v", err)
	}
	full := chains[1].String()

	expect := func(chainID string, wantRoot digest.Digest, wantOK bool) {
		t.Helper()
		m, ok, err := sn.buildableManifest(ctx, manifests, chainID)
		if err != nil {
			t.Fatalf("buildableManifest(%s) error = %v", chainID, err)
		}
		if ok != wantOK || m.root != wantRoot {
			t.Fatalf("buildableManifest(%s) = %s, %v, want %s, %v", chainID, m.root, ok, wantRoot, wantOK)
		}
	}
	expect(full, "", false)
	loadLayers(sn, y)
	expect(full, "", false)
	// Only the first layer's blob decides between the A manifests and alt
	// now: whichever of them has its own blob present is buildable, and the
	// smallest digest among those is chosen even when a smaller one that
	// lacks its blob sorts ahead of it.
	if rootAlt < rootA1 {
		loadLayers(sn, x)
		expect(full, rootA1, true)
		store.blobs[altBlob] = []byte("alternate layer")
		expect(full, rootAlt, true)
	} else {
		store.blobs[altBlob] = []byte("alternate layer")
		expect(full, rootAlt, true)
		loadLayers(sn, x)
		expect(full, rootA1, true)
	}
	expect(chains[0].String(), "", false)
	expect(chainsC[2].String(), "", false)
	loadLayers(sn, w)
	expect(chainsC[2].String(), rootC, true)
	delete(store.blobs, layerBlob(x))
	delete(store.blobs, altBlob)
	expect(full, "", false)
}

// TestLoadedChain covers locating a chain among loaded manifests: a
// manifest ending in it wins over one that continues beyond it, whichever
// sorts first, a chain only inside manifests is answered from the first,
// and a chain none contains is not found.
func TestLoadedChain(t *testing.T) {
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	manifest := func(root string, diffIDs ...digest.Digest) loadedManifest {
		return loadedManifest{root: digest.FromString(root), diffIDs: diffIDs, chains: identity.ChainIDs(slices.Clone(diffIDs))}
	}
	derived := manifest("derived", x, y, w)
	base := manifest("base", x, y)
	other := manifest("other", y)
	chains := derived.chains
	tests := []struct {
		name       string
		manifests  []loadedManifest
		chainID    digest.Digest
		wantDiffID digest.Digest
		wantLast   bool
		wantOK     bool
	}{
		{name: "whole chain", manifests: []loadedManifest{derived}, chainID: chains[2], wantDiffID: w, wantLast: true, wantOK: true},
		{name: "inside an image", manifests: []loadedManifest{derived}, chainID: chains[1], wantDiffID: y, wantOK: true},
		{name: "ending wins over inside", manifests: []loadedManifest{derived, base}, chainID: chains[1], wantDiffID: y, wantLast: true, wantOK: true},
		{name: "first layer", manifests: []loadedManifest{other, derived}, chainID: chains[0], wantDiffID: x, wantOK: true},
		{name: "same diff ID on another chain", manifests: []loadedManifest{derived}, chainID: other.chains[0]},
		{name: "no manifests", chainID: chains[0]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diffID, last, ok := loadedChain(tt.manifests, tt.chainID.String())
			if diffID != tt.wantDiffID || last != tt.wantLast || ok != tt.wantOK {
				t.Fatalf("loadedChain(%s) = %s, %v, %v, want %s, %v, %v", tt.chainID, diffID, last, ok, tt.wantDiffID, tt.wantLast, tt.wantOK)
			}
		})
	}
}

// TestLoadedImageRef covers the reference a built image is recorded under:
// the smallest tag-shaped name, or the manifest digest when every name is
// digest-qualified.
func TestLoadedImageRef(t *testing.T) {
	root := digest.FromString("root")
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{name: "smallest name", names: []string{"registry.example/b:v1", "registry.example/a:v2", "registry.example/a:v1"}, want: "registry.example/a:v1"},
		{name: "digest-qualified names skipped", names: []string{"moby-dangling@" + root.String(), "registry.example/a@" + root.String(), "registry.example/z:v1"}, want: "registry.example/z:v1"},
		{name: "only digest-qualified names", names: []string{"moby-dangling@" + root.String(), "registry.example/a@" + root.String()}, want: root.String()},
		{name: "no names", want: root.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loadedImageRef(loadedManifest{root: root, names: tt.names}); got != tt.want {
				t.Fatalf("loadedImageRef() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHostPlatformConfig covers the platform rule: the host's OS and
// architecture, with an unstated variant standing for the architecture's
// default one.
func TestHostPlatformConfig(t *testing.T) {
	host := hostImagePlatform()
	tests := []struct {
		name     string
		platform ocispec.Platform
		want     bool
	}{
		{name: "host", platform: host, want: true},
		{name: "other os", platform: ocispec.Platform{OS: "plan9", Architecture: host.Architecture}, want: false},
		{name: "other architecture", platform: ocispec.Platform{OS: host.OS, Architecture: "mips"}, want: false},
		{name: "unknown variant", platform: ocispec.Platform{OS: host.OS, Architecture: host.Architecture, Variant: "v0"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostPlatformConfig(&ocispec.Image{Platform: tt.platform}); got != tt.want {
				t.Fatalf("hostPlatformConfig(%+v) = %v, want %v", tt.platform, got, tt.want)
			}
		})
	}

	arm64 := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	arm := ocispec.Platform{OS: "linux", Architecture: "arm"}
	variants := []struct {
		name string
		a, b ocispec.Platform
		want bool
	}{
		{name: "arm64 default variant stated on one side", a: arm64, b: ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, want: true},
		{name: "arm default variant stated on one side", a: arm, b: ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, want: true},
		{name: "arm older variant", a: arm, b: ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v6"}, want: false},
		{name: "amd64 has no default variant", a: ocispec.Platform{OS: "linux", Architecture: "amd64"}, b: ocispec.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}, want: false},
	}
	for _, tt := range variants {
		t.Run(tt.name, func(t *testing.T) {
			if got := samePlatform(tt.a, tt.b); got != tt.want {
				t.Fatalf("samePlatform(%+v, %+v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
