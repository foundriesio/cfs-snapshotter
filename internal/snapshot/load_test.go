// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
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
			sn := newConfiguredSnapshotter(t, Config{Root: t.TempDir(), AllowUnstagedPulls: staged}, fakeEngineForImages(), notMounted)
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
			sn := newConfiguredSnapshotter(t, Config{Root: t.TempDir(), AllowUnstagedPulls: staged}, engine, notMounted)
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
// not hold, in either mode: the Apply is declined with a failed precondition
// that says to sideload the image first, nothing is built and the runtime's
// image records are not read, although they name the image with every blob.
func TestApplyDeclinesUnstoredLoadedChain(t *testing.T) {
	forEachMode(t, func(t *testing.T, sn *snapshotter, engine *fakeEngine) {
		ctx := pullNamespace()
		x := digest.FromString("layer-x")
		addFakeImage(engine, testImageInfo("other", digest.FromString("other-chain")))
		_, chains := loadImage(t, sn, "registry.example/a:v1", x)
		key := loadKey(0, chains[0])
		if _, err := sn.Prepare(ctx, key, ""); err != nil {
			t.Fatalf("Prepare(%s) error = %v", key, err)
		}
		_, err := applyLoaded(ctx, sn, key, x)
		if !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), "ocicfs pull --from-tar") || !strings.Contains(err.Error(), platformHint()) {
			t.Fatalf("Apply(%s) error = %v, want a failed precondition naming ocicfs pull --from-tar and the host's platform", key, err)
		}
		if len(engine.builds) != 0 || len(engine.holds) != 0 || imagesOf(sn).listCount() != 0 {
			t.Fatalf("builds = %+v, holds = %+v, image listings = %d; want nothing", engine.builds, engine.holds, imagesOf(sn).listCount())
		}
		info, err := sn.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", key, err)
		}
		if info.Labels[leafLabel] != "" || info.Labels[stagingLabel] == "" {
			t.Fatalf("record labels = %v, want a plain staging record", info.Labels)
		}
	})
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
// uncancelled context, a collection is asked for, and with no other image to
// answer it the Apply is declined rather than answered as a prefix.
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
	key := loadKey(0, chains[0])
	if _, err := sn.Prepare(ctx, key, ""); err != nil {
		t.Fatalf("Prepare(%s) error = %v", key, err)
	}
	if _, err := applyLoaded(ctx, sn, key, x); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("Apply(%s) error = %v, want the chain declined", key, err)
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
