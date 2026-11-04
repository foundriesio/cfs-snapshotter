// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/errdefs"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// loadKey is the key containerd's client-side unpacker files the record of
// the layer chain under, as it reaches the snapshotter.
func loadKey(n int, chain digest.Digest) string {
	return fmt.Sprintf("moby/%d/extract-%d-abc %s", n, n, chain)
}

func TestUnpackChainID(t *testing.T) {
	chain := digest.FromString("chain")
	tests := []struct {
		key  string
		want string
	}{
		{key: "moby/3/extract-1-abc " + chain.String(), want: chain.String()},
		{key: "extract-1-abc " + chain.String(), want: chain.String()},
		{key: "moby/3/extract-1-abc" + chain.String()},
		{key: "moby/3/extract-1-abc sha512:" + digest.SHA512.FromString("chain").Encoded()},
		{key: "moby/3/extract-1-abc sha256:nothex"},
		{key: "moby/3/0123456789abcdef-init"},
		{key: "moby/extract-1-abc " + chain.String() + "/3/c"},
		{key: "moby/3/prepare-1-abc " + chain.String()},
		{key: ""},
	}
	for _, tt := range tests {
		got, ok := unpackChainID(tt.key)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("unpackChainID(%q) = %q, %v; want %q", tt.key, got, ok, tt.want)
		}
	}
}

// TestPrepareLoadHandshakeStages covers the handshake of a client-side
// unpack, which labels nothing: in either mode it gets a staging record
// labeled with the chain its key names and the placeholder mount, owns no
// directory, and a second unpack of the same chain under another key is
// staged too rather than answered as already present.
func TestPrepareLoadHandshakeStages(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged=%v", staged), func(t *testing.T) {
			ctx := pullNamespace()
			sn := newConfiguredSnapshotter(t, Config{Root: t.TempDir(), AdmitAny: staged}, fakeEngineForImages(), notMounted)
			chain := digest.FromString("chain")
			for n := range 2 {
				key := loadKey(n, chain)
				mounts, err := sn.Prepare(ctx, key, "")
				if err != nil {
					t.Fatalf("Prepare(%s) error = %v", key, err)
				}
				if !isStagingMount(mounts, key) {
					t.Fatalf("mounts = %v, want the staging placeholder naming the record", mounts)
				}
				id, info, err := sn.snapshotInfo(ctx, key)
				if err != nil {
					t.Fatalf("snapshotInfo(%s) error = %v", key, err)
				}
				if info.Labels[stagingLabel] == "" || info.Labels[intermediateSnapshotLabel] != chain.String() {
					t.Fatalf("record labels = %v, want a staging record standing for %s", info.Labels, chain)
				}
				if _, err := os.Stat(sn.getSnapshotDir(id)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("snapshot directory error = %v, want none provisioned", err)
				}
			}
		})
	}
}

// TestPrepareLoadHandshakeNeverPromotes covers an unpack handshake over a
// committed staged record that is no chain member: it is staged like any
// other, with no promotion started from inside the unpack.
func TestPrepareLoadHandshakeNeverPromotes(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	d := pullDerived(t, sn)
	builds, lists := len(engine.builds), engine.lists

	key := loadKey(7, digest.FromString("on-top"))
	mounts, err := sn.Prepare(ctx, key, d.base)
	if err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	if !isStagingMount(mounts, key) {
		t.Fatalf("mounts = %v, want the staging placeholder", mounts)
	}
	if len(engine.builds) != builds || engine.lists != lists || imagesOf(sn).listCount() != 0 {
		t.Fatalf("builds = %d, store lists = %d, image listings = %d; want nothing started", len(engine.builds)-builds, engine.lists-lists, imagesOf(sn).listCount())
	}
	assertUnpromoted(t, sn, d)
}

// storedImageInfo is what the ocicfs store reports for an image whose layers
// have the given diff IDs, along with the chain ID of each layer.
func storedImageInfo(root string, diffIDs ...digest.Digest) (*ocicfs.ImageInfo, []digest.Digest) {
	chains := identity.ChainIDs(slices.Clone(diffIDs))
	layers := make([]ocicfs.LayerInfo, len(diffIDs))
	for i := range diffIDs {
		layers[i] = ocicfs.LayerInfo{DiffID: diffIDs[i], ChainID: chains[i]}
	}
	return &ocicfs.ImageInfo{Root: digest.FromString(root), Layers: layers}, chains
}

// applyLoaded hands the differ the layer with the given diff ID for the
// staging snapshot under key with the whole descriptor, as docker's unpack of
// a loaded image does, and returns the diff descriptor the differ answers.
func applyLoaded(ctx context.Context, sn *snapshotter, key string, diffID digest.Digest) (ocispec.Descriptor, error) {
	return sn.Apply(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: layerBlob(diffID), Size: layerSize}, stagingMount(key))
}

// loadStep runs one layer of a client-side unpack: an unlabeled Prepare under
// the unpack key of chain, the Apply of the layer with the given diff ID,
// which must answer it, and a Commit that passes no labels. It returns the
// committed name.
func loadStep(t *testing.T, sn *snapshotter, n int, parent string, chain, diffID digest.Digest) string {
	t.Helper()
	ctx := pullNamespace()
	key := loadKey(n, chain)
	if _, err := sn.Prepare(ctx, key, parent); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	got, err := applyLoaded(ctx, sn, key, diffID)
	if err != nil {
		t.Fatalf("Apply(%s) error = %v", key, err)
	}
	if want := (ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: diffID}); got.MediaType != want.MediaType || got.Digest != want.Digest {
		t.Fatalf("Apply(%s) = %+v, want %+v", key, got, want)
	}
	name := fmt.Sprintf("moby/%d/%s", n, chain)
	if err := sn.Commit(ctx, name, key); err != nil {
		t.Fatalf("Commit(%s) error = %v", name, err)
	}
	return name
}

// forEachMode runs fn in a snapshotter that stages its pulls and in one that
// does not, each over its own engine.
func forEachMode(t *testing.T, fn func(t *testing.T, sn *snapshotter, engine *fakeEngine)) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged=%v", staged), func(t *testing.T) {
			engine := fakeEngineForImages()
			sn := newConfiguredSnapshotter(t, Config{Root: t.TempDir(), AdmitAny: staged}, engine, notMounted)
			fn(t, sn, engine)
		})
	}
}

// TestApplyResolvesStoredChain covers a loaded image the store already
// holds: the layers below its last are answered with their diff IDs and
// nothing is held or built for them, while the last adopts the stored image
// — held under the build holder of its key, the leaf markers on its record
// and no identity — so its Commit files a chain member a container mounts,
// and removing the record releases the image. The runtime's content store
// is never read, in either mode.
func TestApplyResolvesStoredChain(t *testing.T) {
	forEachMode(t, func(t *testing.T, sn *snapshotter, engine *fakeEngine) {
		ctx := pullNamespace()
		x, y, z := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-z")
		stored, chains := storedImageInfo("stored", x, y, z)
		addFakeImage(engine, stored)

		parent := ""
		for i, diffID := range []digest.Digest{x, y} {
			parent = loadStep(t, sn, i, parent, chains[i], diffID)
			info, err := sn.Stat(ctx, parent)
			if err != nil {
				t.Fatalf("Stat(%s) error = %v", parent, err)
			}
			if info.Labels[chainMemberLabel] != "" || info.Labels[rootDigestLabel] != "" || info.Labels[stagingLabel] == "" {
				t.Fatalf("prefix record labels = %v, want a plain staged record", info.Labels)
			}
		}
		if len(engine.holds) != 0 {
			t.Fatalf("engine holds = %+v, want none below the last layer", engine.holds)
		}

		key := loadKey(2, chains[2])
		if _, err := sn.Prepare(ctx, key, parent); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if _, err := applyLoaded(ctx, sn, key, z); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
		holder := buildHolder("moby", key)
		if want := []holdCall{{ref: stored.Root.String(), holder: holder}}; !slices.Equal(engine.holds, want) {
			t.Fatalf("engine holds = %+v, want %+v", engine.holds, want)
		}
		info, err := sn.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", key, err)
		}
		if info.Labels[leafLabel] == "" || info.Labels[rootDigestLabel] != stored.Root.String() || info.Labels[holderLabel] != holder {
			t.Fatalf("adopted record labels = %v, want the leaf markers naming %s under %s", info.Labels, stored.Root, holder)
		}
		if info.Labels[imageRefLabel] != "" || info.Labels[manifestDigestLabel] != "" {
			t.Fatalf("adopted record labels = %v, want no image identity", info.Labels)
		}
		leaf := fmt.Sprintf("moby/2/%s", chains[2])
		if err := sn.Commit(ctx, leaf, key); err != nil {
			t.Fatalf("Commit(%s) error = %v", leaf, err)
		}
		info, err = sn.Stat(ctx, leaf)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", leaf, err)
		}
		if info.Labels[chainMemberLabel] == "" || info.Labels[intermediateSnapshotLabel] != chains[2].String() {
			t.Fatalf("committed leaf labels = %v, want a chain member standing for %s", info.Labels, chains[2])
		}

		var calls []mountCall
		recordMounts(engine, &calls)
		if _, err := sn.Prepare(ctx, "moby/9/container-init", leaf); err != nil {
			t.Fatalf("Prepare(container-init) error = %v", err)
		}
		if len(calls) != 1 || calls[0].chainID != chains[2].String() {
			t.Fatalf("MountChain calls = %+v, want the stored chain %s", calls, chains[2])
		}
		if len(engine.builds) != 0 || imagesOf(sn).listCount() != 0 {
			t.Fatalf("builds = %+v, image listings = %d, want nothing built and no runtime content read", engine.builds, imagesOf(sn).listCount())
		}
		if err := sn.Remove(ctx, "moby/9/container-init"); err != nil {
			t.Fatalf("Remove(container-init) error = %v", err)
		}
		if err := sn.Remove(ctx, leaf); err != nil {
			t.Fatalf("Remove(%s) error = %v", leaf, err)
		}
		if want := []holdCall{{ref: stored.Root.String(), holder: holder}}; !slices.Equal(engine.releases, want) {
			t.Fatalf("engine releases = %+v, want %+v", engine.releases, want)
		}
	})
}

// TestApplyAdoptsStoredBaseUnderDerivedLoad covers loading a derived image
// whose base the store holds too: the base's chain adopts the base, the
// derived image's adopts the derived image, each under the holder of its
// own key, and a container over either chain mounts it.
func TestApplyAdoptsStoredBaseUnderDerivedLoad(t *testing.T) {
	forEachMode(t, func(t *testing.T, sn *snapshotter, engine *fakeEngine) {
		ctx := pullNamespace()
		x, y, z := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-z")
		base, _ := storedImageInfo("base", x, y)
		derived, chains := storedImageInfo("derived", x, y, z)
		addFakeImage(engine, base)
		addFakeImage(engine, derived)

		names := make([]string, 3)
		parent := ""
		for i, diffID := range []digest.Digest{x, y, z} {
			names[i] = loadStep(t, sn, i, parent, chains[i], diffID)
			parent = names[i]
		}
		want := []holdCall{
			{ref: base.Root.String(), holder: buildHolder("moby", loadKey(1, chains[1]))},
			{ref: derived.Root.String(), holder: buildHolder("moby", loadKey(2, chains[2]))},
		}
		if !slices.Equal(engine.holds, want) {
			t.Fatalf("engine holds = %+v, want %+v", engine.holds, want)
		}
		for i, wantMember := range []bool{false, true, true} {
			info, err := sn.Stat(ctx, names[i])
			if err != nil {
				t.Fatalf("Stat(%s) error = %v", names[i], err)
			}
			if (info.Labels[chainMemberLabel] != "") != wantMember {
				t.Fatalf("record %s labels = %v, want chain member %v", names[i], info.Labels, wantMember)
			}
		}
		var calls []mountCall
		recordMounts(engine, &calls)
		for i, name := range names[1:] {
			if _, err := sn.Prepare(ctx, fmt.Sprintf("moby/9/c%d-init", i), name); err != nil {
				t.Fatalf("Prepare over %s error = %v", name, err)
			}
		}
		if len(calls) != 2 || calls[0].chainID != chains[1].String() || calls[1].chainID != chains[2].String() {
			t.Fatalf("MountChain calls = %+v, want the base chain then the derived one", calls)
		}
		if len(engine.builds) != 0 {
			t.Fatalf("builds = %+v, want none", engine.builds)
		}
	})
}

// TestApplyAnswersPrefixOfStoredImage covers a store holding only the
// derived image: the base's chain is a prefix of it, answered with its diff
// ID and left a plain staged record.
func TestApplyAnswersPrefixOfStoredImage(t *testing.T) {
	forEachMode(t, func(t *testing.T, sn *snapshotter, engine *fakeEngine) {
		x, y, z := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-z")
		derived, chains := storedImageInfo("derived", x, y, z)
		addFakeImage(engine, derived)
		first := loadStep(t, sn, 0, "", chains[0], x)
		second := loadStep(t, sn, 1, first, chains[1], y)
		info, err := sn.Stat(pullNamespace(), second)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", second, err)
		}
		if info.Labels[chainMemberLabel] != "" || len(engine.holds) != 0 {
			t.Fatalf("record labels = %v, holds = %+v; want a plain staged record and nothing held", info.Labels, engine.holds)
		}
	})
}

// TestApplyDeclinesUnstoredLoadedChain covers a loaded image the store does
// not hold on a daemon that builds nothing from runtime content: the Apply
// is declined with a failed precondition that says to sideload the image
// first, nothing is built and the runtime's image records are not read,
// although they name the image with every blob.
func TestApplyDeclinesUnstoredLoadedChain(t *testing.T) {
	ctx := pullNamespace()
	x := digest.FromString("layer-x")
	engine := fakeEngineForImages(testImageInfo("other", digest.FromString("other-chain")))
	sn := newSnapshotterForTest(t, engine, notMounted)
	_, chains := loadImage(t, sn, "registry.example/a:v1", x)
	key := loadKey(0, chains[0])
	if _, err := sn.Prepare(ctx, key, ""); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	_, err := applyLoaded(ctx, sn, key, x)
	if !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), "image unavailable") || !strings.Contains(err.Error(), "ocicfs pull --from-tar") || !strings.Contains(err.Error(), hostPlatform()) {
		t.Fatalf("Apply(%s) error = %v, want a failed precondition naming ocicfs pull --from-tar and the host's platform", key, err)
	}
	if len(engine.builds) != 0 || len(engine.holds) != 0 || imagesOf(sn).listCount() != 0 {
		t.Fatalf("builds = %+v, holds = %+v, image listings = %d; want nothing", engine.builds, engine.holds, imagesOf(sn).listCount())
	}
	assertPlainStagingRecord(t, sn, key)
}

// TestApplyDeclinesUnbuildableLoadedChain covers a staging daemon handed a
// loaded layer the store does not hold and the runtime's content cannot
// build: the image lacks a layer blob, only another platform's image
// provides the chain, or the image records cannot be read. The Apply
// fails, nothing is built or held, and the record stays a plain staging
// record.
func TestApplyDeclinesUnbuildableLoadedChain(t *testing.T) {
	x := digest.FromString("layer-x")
	recordsErr := errors.New("images unavailable")
	tests := []struct {
		name string
		// load puts what the runtime holds in its content store and returns
		// the chain to apply.
		load func(t *testing.T, sn *snapshotter) digest.Digest
		// check judges the Apply's error.
		check func(err error) bool
	}{
		{
			name: "missing layer blob",
			load: func(t *testing.T, sn *snapshotter) digest.Digest {
				root, chains := stageImage(t, sn, x)
				imagesOf(sn).add("registry.example/a:v1", ocispec.MediaTypeImageManifest, root)
				return chains[0]
			},
			check: func(err error) bool {
				return errdefs.IsFailedPrecondition(err) && strings.Contains(err.Error(), "image unavailable") && strings.Contains(err.Error(), "missing layer data") &&
					strings.Contains(err.Error(), "ocicfs pull --from-tar")
			},
		},
		{
			name: "foreign platform only",
			load: func(t *testing.T, sn *snapshotter) digest.Digest {
				root, chains := stageImageConfig(t, sn, ocispec.Image{Platform: ocispec.Platform{OS: "plan9", Architecture: "mips"}}, x)
				loadLayers(sn, x)
				imagesOf(sn).add("registry.example/a:v1", ocispec.MediaTypeImageManifest, root)
				return chains[0]
			},
			check: func(err error) bool {
				return errdefs.IsFailedPrecondition(err) && strings.Contains(err.Error(), "image unavailable") &&
					strings.Contains(err.Error(), "ocicfs pull --from-tar") && strings.Contains(err.Error(), hostPlatform())
			},
		},
		{
			name: "image records unreadable",
			load: func(t *testing.T, sn *snapshotter) digest.Digest {
				_, chains := loadImage(t, sn, "registry.example/a:v1", x)
				imagesOf(sn).err = recordsErr
				return chains[0]
			},
			check: func(err error) bool { return err != nil && strings.Contains(err.Error(), recordsErr.Error()) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := pullNamespace()
			engine := fakeEngineForImages()
			sn := newStagingSnapshotter(t, engine)
			key := loadKey(0, tt.load(t, sn))
			if _, err := sn.Prepare(ctx, key, ""); err != nil {
				t.Fatalf("Prepare(%s) error = %v", key, err)
			}
			if _, err := applyLoaded(ctx, sn, key, x); !tt.check(err) {
				t.Fatalf("Apply(%s) error = %v, want the %s declined", key, err, tt.name)
			}
			if len(engine.builds) != 0 || len(engine.holds) != 0 {
				t.Fatalf("builds = %+v, holds = %+v; want nothing", engine.builds, engine.holds)
			}
			assertPlainStagingRecord(t, sn, key)
		})
	}
}

// assertPlainStagingRecord checks that the record under key is a staging
// record carrying no leaf markers.
func assertPlainStagingRecord(t *testing.T, sn *snapshotter, key string) {
	t.Helper()
	info, err := sn.Stat(pullNamespace(), key)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", key, err)
	}
	if info.Labels[leafLabel] != "" || info.Labels[rootDigestLabel] != "" || info.Labels[holderLabel] != "" || info.Labels[stagingLabel] == "" {
		t.Fatalf("record labels = %v, want a plain staging record", info.Labels)
	}
}

// TestApplyBuildsLoadedChain covers a staging daemon loading an image the
// store does not hold: the layers below the last are answered with the
// manifest's diff IDs, with nothing built or held, while the last builds the
// image whole from the runtime's content store — recorded under the loaded
// name, rooted at its manifest and held under the build holder of its key —
// and carries the leaf markers, so its Commit files a chain member a
// container mounts and removing the record releases the image.
func TestApplyBuildsLoadedChain(t *testing.T) {
	ctx := pullNamespace()
	x, y, z := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-z")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := loadImage(t, sn, "registry.example/a:v1", x, y, z)

	parent := ""
	for i, diffID := range []digest.Digest{x, y} {
		parent = loadStep(t, sn, i, parent, chains[i], diffID)
	}
	if len(engine.builds) != 0 || len(engine.holds) != 0 {
		t.Fatalf("builds = %+v, holds = %+v, want none below the last layer", engine.builds, engine.holds)
	}

	key := loadKey(2, chains[2])
	if _, err := sn.Prepare(ctx, key, parent); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	got, err := applyLoaded(ctx, sn, key, z)
	if err != nil {
		t.Fatalf("Apply(%s) error = %v", key, err)
	}
	if got.Digest != z {
		t.Fatalf("Apply(%s) = %+v, want the diff ID %s", key, got, z)
	}
	holder := buildHolder("moby", key)
	if len(engine.builds) != 1 {
		t.Fatalf("builds = %+v, want one", engine.builds)
	}
	if b := engine.builds[0]; b.ref != "registry.example/a:v1" || b.opts.ResolvedRoot != root || b.opts.Hold != holder ||
		b.opts.BlobSource != ocicfs.BlobSource(sn.blobs) {
		t.Fatalf("NewBuild(%q, %+v), want the loaded image rooted at %s held under %s from the content store", b.ref, b.opts, root, holder)
	}
	if calls := engine.callLog(); !slices.Equal(calls, []string{"build0.finish"}) {
		t.Fatalf("engine calls = %v, want the build finished whole", calls)
	}
	if want := []holdCall{{ref: root.String(), holder: holder}}; !slices.Equal(engine.holds, want) {
		t.Fatalf("engine holds = %+v, want %+v", engine.holds, want)
	}
	if len(sn.builds) != 0 || len(sn.finishing) != 0 {
		t.Fatalf("open builds = %d, finishing = %d, want none", len(sn.builds), len(sn.finishing))
	}
	info, err := sn.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", key, err)
	}
	if info.Labels[leafLabel] == "" || info.Labels[rootDigestLabel] != root.String() || info.Labels[holderLabel] != holder {
		t.Fatalf("built record labels = %v, want the leaf markers naming %s under %s", info.Labels, root, holder)
	}
	leaf := fmt.Sprintf("moby/2/%s", chains[2])
	if err := sn.Commit(ctx, leaf, key); err != nil {
		t.Fatalf("Commit(%s) error = %v", leaf, err)
	}
	info, err = sn.Stat(ctx, leaf)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", leaf, err)
	}
	if info.Labels[chainMemberLabel] == "" || info.Labels[intermediateSnapshotLabel] != chains[2].String() {
		t.Fatalf("committed leaf labels = %v, want a chain member standing for %s", info.Labels, chains[2])
	}

	addFakeImage(engine, &ocicfs.ImageInfo{Root: root, Layers: []ocicfs.LayerInfo{{DiffID: z, ChainID: chains[2]}}})
	var calls []mountCall
	recordMounts(engine, &calls)
	if _, err := sn.Prepare(ctx, "moby/9/container-init", leaf); err != nil {
		t.Fatalf("Prepare(container-init) error = %v", err)
	}
	if len(calls) != 1 || calls[0].chainID != chains[2].String() {
		t.Fatalf("MountChain calls = %+v, want the built chain %s", calls, chains[2])
	}
	if err := sn.Remove(ctx, "moby/9/container-init"); err != nil {
		t.Fatalf("Remove(container-init) error = %v", err)
	}
	if err := sn.Remove(ctx, leaf); err != nil {
		t.Fatalf("Remove(%s) error = %v", leaf, err)
	}
	if want := []holdCall{{ref: root.String(), holder: holder}}; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine releases = %+v, want %+v", engine.releases, want)
	}
}

// TestApplyBuildsEachLoadedImageAtItsChain covers an archive holding a base
// image and one derived from it, the derived unpacked first: the base's
// chain ends an image, so it builds the base there rather than being
// answered as a layer of the derived image, and the derived image's last
// layer builds the derived image.
func TestApplyBuildsEachLoadedImageAtItsChain(t *testing.T) {
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	rootB, chains := loadImage(t, sn, "registry.example/b:v1", x, y, w)
	rootA, _ := loadImage(t, sn, "registry.example/a:v1", x, y)

	parent := ""
	for i, diffID := range []digest.Digest{x, y, w} {
		parent = loadStep(t, sn, i, parent, chains[i], diffID)
	}
	want := []holdCall{
		{ref: rootA.String(), holder: buildHolder("moby", loadKey(1, chains[1]))},
		{ref: rootB.String(), holder: buildHolder("moby", loadKey(2, chains[2]))},
	}
	if !slices.Equal(engine.holds, want) {
		t.Fatalf("engine holds = %+v, want %+v", engine.holds, want)
	}
	if len(engine.builds) != 2 || engine.builds[0].ref != "registry.example/a:v1" || engine.builds[1].ref != "registry.example/b:v1" {
		t.Fatalf("builds = %+v, want the base then the derived image", engine.builds)
	}
	for i, wantMember := range []bool{false, true, true} {
		name := fmt.Sprintf("moby/%d/%s", i, chains[i])
		info, err := sn.Stat(pullNamespace(), name)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", name, err)
		}
		if (info.Labels[chainMemberLabel] != "") != wantMember {
			t.Fatalf("record %s labels = %v, want chain member %v", name, info.Labels, wantMember)
		}
	}
}

// TestApplyAnswersChainOfIncompleteLoadedImageAsPrefix covers an archive
// holding a base image whose layer blobs the content store lacks, under
// another compression, and a complete image derived from it: the base's
// chain cannot build the base, so it is answered as a layer of the derived
// image, and the derived image's last layer builds the derived image.
func TestApplyAnswersChainOfIncompleteLoadedImageAsPrefix(t *testing.T) {
	x, y, z := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-z")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	rootB, chains := loadImage(t, sn, "registry.example/b:v1", x, y, z)
	staged, _ := stageImageBy(t, sn, "a", x, y)
	store := contentOf(sn)
	var manifest ocispec.Manifest
	if err := json.Unmarshal(store.blobs[staged], &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	for i := range manifest.Layers {
		manifest.Layers[i].Digest = digest.FromString("zstd:" + manifest.Layers[i].Digest.String())
	}
	blob, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	store.blobs[digest.FromBytes(blob)] = blob
	imagesOf(sn).add("registry.example/a:v1", ocispec.MediaTypeImageManifest, digest.FromBytes(blob))

	parent := ""
	for i, diffID := range []digest.Digest{x, y} {
		parent = loadStep(t, sn, i, parent, chains[i], diffID)
	}
	if len(engine.builds) != 0 || len(engine.holds) != 0 {
		t.Fatalf("builds = %+v, holds = %+v, want none below the derived image's last layer", engine.builds, engine.holds)
	}
	loadStep(t, sn, 2, parent, chains[2], z)
	want := []holdCall{{ref: rootB.String(), holder: buildHolder("moby", loadKey(2, chains[2]))}}
	if !slices.Equal(engine.holds, want) || len(engine.builds) != 1 || engine.builds[0].ref != "registry.example/b:v1" {
		t.Fatalf("builds = %+v, holds = %+v, want the derived image built and held as %+v", engine.builds, engine.holds, want)
	}
}

// TestLoadedLeafFailureReleases covers a loaded image whose build fails with
// its hold written, or whose record cannot be marked once it is built, under
// a caller that has cancelled meanwhile: the hold is released all the same,
// on a context the cancellation does not reach, a collection is asked for,
// the Apply fails, and the record stays a plain staging record.
func TestLoadedLeafFailureReleases(t *testing.T) {
	failure := errors.New("injected failure")
	tests := []struct {
		name string
		// inject arranges the failure; cancel cancels the Apply's caller.
		// A gated failure blocks the build's Finish until the caller has
		// cancelled.
		inject func(sn *snapshotter, engine *fakeEngine, cancel context.CancelFunc)
		gated  bool
	}{
		{
			name: "build fails with its hold written",
			inject: func(_ *snapshotter, engine *fakeEngine, _ context.CancelFunc) {
				engine.finishHeldErr = failure
				engine.finishGate = make(chan struct{})
			},
			gated: true,
		},
		{
			name: "marking the record fails",
			inject: func(sn *snapshotter, _ *fakeEngine, cancel context.CancelFunc) {
				sn.updateInfo = func(context.Context, snapshots.Info, ...string) (snapshots.Info, error) {
					cancel()
					return snapshots.Info{}, failure
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := fakeEngineForImages()
			sn := newStagingSnapshotter(t, engine)
			awaitGCs(t, engine, 1)
			x := digest.FromString("layer-x")
			root, chains := loadImage(t, sn, "registry.example/a:v1", x)
			key := loadKey(0, chains[0])
			if _, err := sn.Prepare(pullNamespace(), key, ""); err != nil {
				t.Fatalf("Prepare(%s) error = %v", key, err)
			}
			ctx, cancel := context.WithCancel(pullNamespace())
			defer cancel()
			tt.inject(sn, engine, cancel)
			done := make(chan error, 1)
			go func() {
				_, err := applyLoaded(ctx, sn, key, x)
				done <- err
			}()
			if tt.gated {
				awaitCall(t, engine, "build0.finish")
				cancel()
				engine.finishGateOnce.Do(func() { close(engine.finishGate) })
			}
			if err := <-done; !errors.Is(err, failure) {
				t.Fatalf("Apply(%s) error = %v, want %v", key, err, failure)
			}
			if ctx.Err() == nil {
				t.Fatal("the caller's context was not cancelled before the failure")
			}
			if want := []holdCall{{ref: root.String(), holder: buildHolder("moby", key)}}; !slices.Equal(engine.releases, want) {
				t.Fatalf("engine releases = %+v, want %+v", engine.releases, want)
			}
			if engine.releaseCtxErrs[0] != nil {
				t.Fatalf("Release context error = %v, want a live context", engine.releaseCtxErrs[0])
			}
			awaitGCs(t, engine, 2)
			sn.updateInfo = storage.UpdateInfo
			assertPlainStagingRecord(t, sn, key)
		})
	}
}

// TestLoadedLeafRemovedDuringBuild covers the unpacker unwinding a loaded
// layer while its image builds: the Remove finds no hold on the record to
// release, the build's hold lands afterwards, and the marking that follows
// fails on the vanished record and releases that hold.
func TestLoadedLeafRemovedDuringBuild(t *testing.T) {
	ctx := pullNamespace()
	x := digest.FromString("layer-x")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := loadImage(t, sn, "registry.example/a:v1", x)
	key := loadKey(0, chains[0])
	if _, err := sn.Prepare(ctx, key, ""); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	engine.finishGate = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := applyLoaded(ctx, sn, key, x)
		done <- err
	}()
	awaitCall(t, engine, "build0.finish")
	if err := sn.Remove(ctx, key); err != nil {
		t.Fatalf("Remove(%s) error = %v", key, err)
	}
	if len(engine.releases) != 0 {
		t.Fatalf("engine releases = %+v after the Remove, want none", engine.releases)
	}
	engine.finishGateOnce.Do(func() { close(engine.finishGate) })
	if err := <-done; !errdefs.IsNotFound(err) {
		t.Fatalf("Apply(%s) error = %v, want the vanished record not found", key, err)
	}
	if want := []holdCall{{ref: root.String(), holder: buildHolder("moby", key)}}; !slices.Equal(engine.holds, want) || !slices.Equal(engine.releases, want) {
		t.Fatalf("holds = %+v, releases = %+v, want the build's hold taken and released once", engine.holds, engine.releases)
	}
}

// TestApplyAnswersStoredPrefixOfLoadedImage covers a base loaded alone onto
// a store that holds only an image derived from it: every chain of the base
// is a prefix of the stored image and is answered from the store, so the
// load builds nothing, even at the base's last layer, and leaves that record
// plain. The first container over the record promotes it by building the
// base from the content its load left.
func TestApplyAnswersStoredPrefixOfLoadedImage(t *testing.T) {
	ctx := pullNamespace()
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	derived, chains := storedImageInfo("derived", x, y, w)
	addFakeImage(engine, derived)
	rootA, _ := loadImage(t, sn, "registry.example/a:v1", x, y)

	first := loadStep(t, sn, 0, "", chains[0], x)
	leaf := loadStep(t, sn, 1, first, chains[1], y)
	if len(engine.builds) != 0 || len(engine.holds) != 0 || imagesOf(sn).listCount() != 0 {
		t.Fatalf("builds = %+v, holds = %+v, image listings = %d; want nothing built and no runtime content read at the load", engine.builds, engine.holds, imagesOf(sn).listCount())
	}
	info, err := sn.Stat(ctx, leaf)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", leaf, err)
	}
	if info.Labels[chainMemberLabel] != "" || info.Labels[rootDigestLabel] != "" || info.Labels[stagingLabel] == "" {
		t.Fatalf("base leaf labels = %v, want a plain staged record", info.Labels)
	}

	recordMounts(engine, &[]mountCall{})
	if _, err := sn.Prepare(ctx, "moby/9/container-init", leaf); err != nil {
		t.Fatalf("Prepare(container-init) error = %v", err)
	}
	holder := recordHolder(leaf)
	if len(engine.builds) != 1 {
		t.Fatalf("builds = %+v, want the promotion's alone", engine.builds)
	}
	if b := engine.builds[0]; b.ref != "registry.example/a:v1" || b.opts.ResolvedRoot != rootA || b.opts.Hold != holder ||
		b.opts.BlobSource != ocicfs.BlobSource(sn.blobs) {
		t.Fatalf("promotion NewBuild(%q, %+v), want the base rooted at %s held under %s from the content store", b.ref, b.opts, rootA, holder)
	}
	info, err = sn.Stat(ctx, leaf)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", leaf, err)
	}
	if info.Labels[chainMemberLabel] == "" || info.Labels[rootDigestLabel] != rootA.String() || info.Labels[holderLabel] != holder {
		t.Fatalf("promoted record labels = %v, want a chain member holding the base %s under %s", info.Labels, rootA, holder)
	}
}

// TestLoadedLeafRetiresPullBuild covers a pull of A = (x, y) halfway through
// when a load builds another image ending in (x, y): the record committed at
// A's whole chain means A's pull will never apply the layer that finishes
// its build, so that build is closed and a collection asked for.
func TestLoadedLeafRetiresPullBuild(t *testing.T) {
	x, y := digest.FromString("layer-x"), digest.FromString("layer-y")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	rootA, chainsA := stageImageBy(t, sn, "a", x, y)
	stagedStep(t, sn, "moby/0/extract", "", "registry.example/a:v1", rootA, chainsA[0], x)
	if len(sn.builds) != 1 {
		t.Fatalf("open builds = %v, want A's", sn.builds)
	}
	_, chains := loadImageBy(t, sn, "registry.example/l:v1", "l", x, y)

	first := loadStep(t, sn, 5, "", chains[0], x)
	loadStep(t, sn, 6, first, chains[1], y)
	if len(sn.builds) != 0 {
		t.Fatalf("open builds = %v, want none", sn.builds)
	}
	if calls := engine.callLog(); !slices.Contains(calls, "build0.close") || slices.Contains(calls, "build0.finish") || !slices.Contains(calls, "build1.finish") {
		t.Fatalf("engine calls = %v, want A's build closed unfinished and the loaded image built", calls)
	}
	awaitGCs(t, engine, 2)
}

// TestConcurrentLoadedBuildsConverge covers the colliding Commit of two
// loads of one image that both reached its last chain: each built it under
// the holder of its own key, one Commit wins, and removing the loser
// releases only its own hold.
func TestConcurrentLoadedBuildsConverge(t *testing.T) {
	ctx := pullNamespace()
	x := digest.FromString("layer-x")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	root, chains := loadImage(t, sn, "registry.example/a:v1", x)
	keys := []string{loadKey(0, chains[0]), loadKey(1, chains[0])}
	for _, key := range keys {
		if _, err := sn.Prepare(ctx, key, ""); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if _, err := applyLoaded(ctx, sn, key, x); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
	}
	if len(engine.builds) != 2 || len(engine.holds) != 2 {
		t.Fatalf("builds = %+v, holds = %+v, want one of each per load", engine.builds, engine.holds)
	}
	name := "moby/0/" + chains[0].String()
	if err := sn.Commit(ctx, name, keys[0]); err != nil {
		t.Fatalf("Commit(%s) error = %v", keys[0], err)
	}
	if err := sn.Commit(ctx, name, keys[1]); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Commit(%s) error = %v, want already exists", keys[1], err)
	}
	if err := sn.Remove(ctx, keys[1]); err != nil {
		t.Fatalf("Remove(%s) error = %v", keys[1], err)
	}
	if want := []holdCall{{ref: root.String(), holder: buildHolder("moby", keys[1])}}; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine releases = %+v, want the loser's hold alone %+v", engine.releases, want)
	}
}

// TestApplyLoadedChainSkipsUnreadableImage covers a stored image that cannot
// be inspected beside a healthy one: the healthy one still answers, while a
// store that cannot be listed fails the Apply.
func TestApplyLoadedChainSkipsUnreadableImage(t *testing.T) {
	ctx := pullNamespace()
	x, y := digest.FromString("layer-x"), digest.FromString("layer-y")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	healthy, chains := storedImageInfo("healthy", x, y)
	broken, _ := storedImageInfo("broken", x, y)
	addFakeImage(engine, healthy)
	addFakeImage(engine, broken)
	engine.inspectFunc = func(_ context.Context, ref string) (*ocicfs.ImageInfo, error) {
		if ref == broken.Root.String() {
			return nil, errors.New("binding unreadable")
		}
		return engine.infos[ref], nil
	}
	first := loadStep(t, sn, 0, "", chains[0], x)
	loadStep(t, sn, 1, first, chains[1], y)
	if want := []holdCall{{ref: healthy.Root.String(), holder: buildHolder("moby", loadKey(1, chains[1]))}}; !slices.Equal(engine.holds, want) {
		t.Fatalf("engine holds = %+v, want %+v", engine.holds, want)
	}

	listErr := errors.New("store unreadable")
	engine.listErr = listErr
	key := loadKey(5, chains[0])
	if _, err := sn.Prepare(ctx, key, ""); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	if _, err := applyLoaded(ctx, sn, key, x); !errors.Is(err, listErr) {
		t.Fatalf("Apply(%s) error = %v, want %v", key, err, listErr)
	}
}

// TestApplyLoadedChainRemovalRace covers the only stored image ending in the
// chain removed between the scan and the hold: the hold is released on an
// uncancelled context, a collection is asked for, and with no other stored
// image to answer it and none loaded the Apply is declined rather than
// answered as a prefix.
func TestApplyLoadedChainRemovalRace(t *testing.T) {
	ctx := pullNamespace()
	x := digest.FromString("layer-x")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	stored, chains := storedImageInfo("stored", x)
	addFakeImage(engine, stored)
	engine.inspectFunc = func(_ context.Context, ref string) (*ocicfs.ImageInfo, error) {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		if len(engine.holds) > 0 {
			return nil, fmt.Errorf("inspect %s: %w", ref, ocicfs.ErrNotFound)
		}
		return engine.infos[ref], nil
	}
	// The runtime has loaded nothing.
	imagesOf(sn)
	key := loadKey(0, chains[0])
	if _, err := sn.Prepare(ctx, key, ""); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	if _, err := applyLoaded(ctx, sn, key, x); !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), "no loaded image for this host has this chain") {
		t.Fatalf("Apply(%s) error = %v, want the chain declined as provided by no loaded image", key, err)
	}
	want := []holdCall{{ref: stored.Root.String(), holder: buildHolder("moby", key)}}
	if !slices.Equal(engine.holds, want) || !slices.Equal(engine.releases, want) {
		t.Fatalf("holds = %+v, releases = %+v, want the vanished image held and released once", engine.holds, engine.releases)
	}
	if engine.releaseCtxErrs[0] != nil {
		t.Fatalf("Release context error = %v, want none", engine.releaseCtxErrs[0])
	}
	awaitGCs(t, engine, 2)
}

// TestLoadedPrefixRetiresUnfinishedPullBuild covers a pull of A = (x, y)
// halfway through when a load answers (x, y) as a prefix of a stored image:
// the record committed at A's whole chain means A's pull will never apply
// the layer that finishes its build, so that build is closed and a
// collection asked for, while a pull ending in a longer chain keeps its
// build.
func TestLoadedPrefixRetiresUnfinishedPullBuild(t *testing.T) {
	x, y, z, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-z"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	stored, chains := storedImageInfo("stored", x, y, z)
	addFakeImage(engine, stored)
	rootA, chainsA := stageImageBy(t, sn, "a", x, y)
	rootC, chainsC := stageImageBy(t, sn, "c", x, y, w)
	stagedStep(t, sn, "moby/0/extract", "", "registry.example/a:v1", rootA, chainsA[0], x)
	stagedStep(t, sn, "moby/1/extract", "", "registry.example/c:v1", rootC, chainsC[0], x)
	if len(sn.builds) != 2 {
		t.Fatalf("open builds = %v, want A's and C's", sn.builds)
	}

	first := loadStep(t, sn, 5, "", chains[0], x)
	loadStep(t, sn, 6, first, chains[1], y)
	if _, ok := sn.builds[buildKey{namespace: "moby", root: rootC.String()}]; !ok || len(sn.builds) != 1 {
		t.Fatalf("open builds = %v, want C's alone", sn.builds)
	}
	if calls := engine.callLog(); !slices.Contains(calls, "build0.close") || slices.Contains(calls, "build0.finish") || slices.Contains(calls, "build1.close") {
		t.Fatalf("engine calls = %v, want A's build closed unfinished and C's left open", calls)
	}
	awaitGCs(t, engine, 2)
}

// TestAdoptedLoadedLeafRemovedUncommitted covers the unpacker unwinding an
// adopted layer before its Commit: removing the record releases the hold it
// took, and there is no build to close.
func TestAdoptedLoadedLeafRemovedUncommitted(t *testing.T) {
	forEachMode(t, func(t *testing.T, sn *snapshotter, engine *fakeEngine) {
		ctx := pullNamespace()
		x := digest.FromString("layer-x")
		stored, chains := storedImageInfo("stored", x)
		addFakeImage(engine, stored)
		key := loadKey(0, chains[0])
		if _, err := sn.Prepare(ctx, key, ""); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if _, err := applyLoaded(ctx, sn, key, x); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
		if err := sn.Remove(ctx, key); err != nil {
			t.Fatalf("Remove(%s) error = %v", key, err)
		}
		if want := []holdCall{{ref: stored.Root.String(), holder: buildHolder("moby", key)}}; !slices.Equal(engine.releases, want) {
			t.Fatalf("engine releases = %+v, want %+v", engine.releases, want)
		}
		if len(engine.callLog()) != 1 {
			t.Fatalf("engine calls = %v, want the hold alone", engine.callLog())
		}
	})
}

// TestConcurrentLoadedAdoptionsConverge covers two unpacks of one stored
// image racing to its last chain: each adopts it under the holder of its own
// key, one Commit wins, and removing the loser releases only its own hold.
func TestConcurrentLoadedAdoptionsConverge(t *testing.T) {
	ctx := pullNamespace()
	x := digest.FromString("layer-x")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	stored, chains := storedImageInfo("stored", x)
	addFakeImage(engine, stored)
	keys := []string{loadKey(0, chains[0]), loadKey(1, chains[0])}
	for _, key := range keys {
		if _, err := sn.Prepare(ctx, key, ""); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		if _, err := applyLoaded(ctx, sn, key, x); err != nil {
			t.Fatalf("Apply(%s) error = %v", key, err)
		}
	}
	name := "moby/0/" + chains[0].String()
	if err := sn.Commit(ctx, name, keys[0]); err != nil {
		t.Fatalf("Commit(%s) error = %v", keys[0], err)
	}
	if err := sn.Commit(ctx, name, keys[1]); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Commit(%s) error = %v, want already exists", keys[1], err)
	}
	if err := sn.Remove(ctx, keys[1]); err != nil {
		t.Fatalf("Remove(%s) error = %v", keys[1], err)
	}
	if want := []holdCall{{ref: stored.Root.String(), holder: buildHolder("moby", keys[1])}}; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine releases = %+v, want the loser's hold alone %+v", engine.releases, want)
	}
}

// TestApplyHalfLabeledRecordStaysOnPullPath covers a record that names an
// image but no manifest: it is a pull's record with a label missing, not a
// loaded layer, and is refused as before even though the store holds its
// chain whole.
func TestApplyHalfLabeledRecordStaysOnPullPath(t *testing.T) {
	ctx := pullNamespace()
	x := digest.FromString("layer-x")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	stored, chains := storedImageInfo("stored", x)
	addFakeImage(engine, stored)
	labels := map[string]string{intermediateSnapshotLabel: chains[0].String(), imageRefLabel: "registry.example/a:v1"}
	if _, err := sn.Prepare(ctx, "moby/0/extract", "", snapshots.WithLabels(labels)); err != nil {
		t.Fatalf("Prepare error = %v", err)
	}
	if _, err := applyLoaded(ctx, sn, "moby/0/extract", x); !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), "names no manifest") {
		t.Fatalf("Apply error = %v, want the pull path's missing-manifest refusal", err)
	}
	if len(engine.holds) != 0 || len(engine.builds) != 0 {
		t.Fatalf("holds = %+v, builds = %+v, want nothing", engine.holds, engine.builds)
	}
}

// TestPreparePromotionAdoptsLateStoredBase covers a base sideloaded after
// its derived image was loaded: the load left the base's chain a plain
// staged record, as a prefix of the derived image, and the first container
// over it promotes it by adopting the stored base — held under the record's
// own holder, published as a chain member named by the base's digest — with
// nothing built and the runtime's content not read, in either mode.
// Removing the record releases the base.
func TestPreparePromotionAdoptsLateStoredBase(t *testing.T) {
	forEachMode(t, func(t *testing.T, sn *snapshotter, engine *fakeEngine) {
		ctx := pullNamespace()
		x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
		derived, chains := storedImageInfo("derived", x, y, w)
		addFakeImage(engine, derived)
		names := make([]string, 3)
		parent := ""
		for i, diffID := range []digest.Digest{x, y, w} {
			names[i] = loadStep(t, sn, i, parent, chains[i], diffID)
			parent = names[i]
		}
		base, _ := storedImageInfo("base", x, y)
		addFakeImage(engine, base)

		var calls []mountCall
		recordMounts(engine, &calls)
		if _, err := sn.Prepare(ctx, "moby/9/container-init", names[1]); err != nil {
			t.Fatalf("Prepare(container-init) error = %v", err)
		}
		holder := recordHolder(names[1])
		if last := engine.holds[len(engine.holds)-1]; last != (holdCall{ref: base.Root.String(), holder: holder}) {
			t.Fatalf("engine holds = %+v, want the base held under %s last", engine.holds, holder)
		}
		info, err := sn.Stat(ctx, names[1])
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", names[1], err)
		}
		want := map[string]string{
			chainMemberLabel:    "true",
			rootDigestLabel:     base.Root.String(),
			holderLabel:         holder,
			imageRefLabel:       base.Root.String(),
			manifestDigestLabel: base.Root.String(),
		}
		for k, v := range want {
			if info.Labels[k] != v {
				t.Fatalf("promoted record labels = %v, want %s = %s", info.Labels, k, v)
			}
		}
		if info.Labels[stagingLabel] != "" {
			t.Fatalf("promoted record labels = %v, want the staging marker dropped", info.Labels)
		}
		if len(calls) != 1 || calls[0].chainID != chains[1].String() {
			t.Fatalf("MountChain calls = %+v, want the base chain %s", calls, chains[1])
		}
		if len(engine.builds) != 0 || imagesOf(sn).listCount() != 0 {
			t.Fatalf("builds = %+v, image listings = %d, want nothing built and no runtime content read", engine.builds, imagesOf(sn).listCount())
		}
		if err := sn.Remove(ctx, "moby/9/container-init"); err != nil {
			t.Fatalf("Remove(container-init) error = %v", err)
		}
		if err := sn.Remove(ctx, names[2]); err != nil {
			t.Fatalf("Remove(%s) error = %v", names[2], err)
		}
		if err := sn.Remove(ctx, names[1]); err != nil {
			t.Fatalf("Remove(%s) error = %v", names[1], err)
		}
		if !slices.Contains(engine.releases, holdCall{ref: base.Root.String(), holder: holder}) {
			t.Fatalf("engine releases = %+v, want the promotion's hold released", engine.releases)
		}
	})
}

// TestPreparePromotionBuildsLoadOriginBase covers a base loaded after the
// image derived from it, on a staging daemon with nothing sideloaded: the
// derived load built the derived image and left the base's chain a plain
// staged record, as a layer inside it, and the base's load found the chain
// committed and applied nothing. The first container over the base's
// record promotes it by building the base whole from the runtime's content
// store, held under the record's own holder.
func TestPreparePromotionBuildsLoadOriginBase(t *testing.T) {
	ctx := pullNamespace()
	x, y, w := digest.FromString("layer-x"), digest.FromString("layer-y"), digest.FromString("layer-w")
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	rootB, chains := loadImage(t, sn, "registry.example/b:v1", x, y, w)
	names := make([]string, 3)
	parent := ""
	for i, diffID := range []digest.Digest{x, y, w} {
		names[i] = loadStep(t, sn, i, parent, chains[i], diffID)
		parent = names[i]
	}
	if len(engine.builds) != 1 || engine.builds[0].opts.ResolvedRoot != rootB {
		t.Fatalf("builds = %+v, want the derived image alone", engine.builds)
	}
	assertPlainStagingRecord(t, sn, names[1])
	rootA, _ := loadImage(t, sn, "registry.example/a:v1", x, y)

	var calls []mountCall
	recordMounts(engine, &calls)
	if _, err := sn.Prepare(ctx, "moby/9/container-init", names[1]); err != nil {
		t.Fatalf("Prepare(container-init) error = %v", err)
	}
	holder := recordHolder(names[1])
	if len(engine.builds) != 2 {
		t.Fatalf("builds = %+v, want the derived image's and the promotion's", engine.builds)
	}
	if b := engine.builds[1]; b.ref != "registry.example/a:v1" || b.opts.ResolvedRoot != rootA || b.opts.Hold != holder {
		t.Fatalf("promotion NewBuild(%q, %+v), want the base rooted at %s held under %s", b.ref, b.opts, rootA, holder)
	}
	info, err := sn.Stat(ctx, names[1])
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", names[1], err)
	}
	want := map[string]string{
		chainMemberLabel:    "true",
		rootDigestLabel:     rootA.String(),
		holderLabel:         holder,
		imageRefLabel:       "registry.example/a:v1",
		manifestDigestLabel: rootA.String(),
	}
	for k, v := range want {
		if info.Labels[k] != v {
			t.Fatalf("promoted record labels = %v, want %s = %s", info.Labels, k, v)
		}
	}
	if len(calls) != 1 || calls[0].chainID != chains[1].String() {
		t.Fatalf("MountChain calls = %+v, want the base chain %s", calls, chains[1])
	}
}

// TestPreparePromotionPrefersStore covers a staging daemon whose store holds
// the base a derived pull left unbuilt while the runtime's content could
// build it too: the stored base is adopted and nothing is built.
func TestPreparePromotionPrefersStore(t *testing.T) {
	ctx := pullNamespace()
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	d := pullDerived(t, sn)
	loadImage(t, sn, "registry.example/a:v1", d.x, d.y)
	base, _ := storedImageInfo("base", d.x, d.y)
	addFakeImage(engine, base)
	recordMounts(engine, &[]mountCall{})

	if _, err := sn.Prepare(ctx, "moby/9/container-init", d.base); err != nil {
		t.Fatalf("Prepare(container-init) error = %v", err)
	}
	if len(engine.builds) != 1 || imagesOf(sn).listCount() != 0 {
		t.Fatalf("builds = %+v, image listings = %d, want the derived pull's build alone and no runtime records read", engine.builds, imagesOf(sn).listCount())
	}
	info, err := sn.Stat(ctx, d.base)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", d.base, err)
	}
	if info.Labels[chainMemberLabel] == "" || info.Labels[rootDigestLabel] != base.Root.String() {
		t.Fatalf("promoted record labels = %v, want a chain member holding the stored base", info.Labels)
	}
}

// TestPreparePromotionAdoptionReleasesOnPublishFailure covers an adopted base
// whose publication fails: the hold is released on an uncancelled context, a
// collection is asked for, the Prepare fails, and the record is left as it
// was.
func TestPreparePromotionAdoptionReleasesOnPublishFailure(t *testing.T) {
	engine := fakeEngineForImages()
	sn := newStagingSnapshotter(t, engine)
	awaitGCs(t, engine, 1)
	d := pullDerived(t, sn)
	base, _ := storedImageInfo("base", d.x, d.y)
	addFakeImage(engine, base)
	recordMounts(engine, &[]mountCall{})
	sn.updateInfo = func(context.Context, snapshots.Info, ...string) (snapshots.Info, error) {
		return snapshots.Info{}, errors.New("publish failed")
	}

	ctx, cancel := context.WithCancel(pullNamespace())
	defer cancel()
	if _, err := sn.Prepare(ctx, "moby/9/container-init", d.base); err == nil || !strings.Contains(err.Error(), "publish failed") {
		t.Fatalf("Prepare(container-init) error = %v, want the publish failure", err)
	}
	if want := []holdCall{{ref: base.Root.String(), holder: recordHolder(d.base)}}; !slices.Equal(engine.releases, want) {
		t.Fatalf("engine releases = %+v, want %+v", engine.releases, want)
	}
	if engine.releaseCtxErrs[0] != nil {
		t.Fatalf("Release context error = %v, want none", engine.releaseCtxErrs[0])
	}
	awaitGCs(t, engine, 2)
	assertUnpromoted(t, sn, d)
	if len(engine.builds) != 1 {
		t.Fatalf("builds = %+v, want the derived pull's alone", engine.builds)
	}
}
