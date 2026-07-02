// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/foundriesio/knarr/pkg/knarr"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
)

// testObjectsDir is the knarr object directory the fake engine reports; it
// appears as the data-only lower in expected overlay specs.
const testObjectsDir = "/knarr/objects/sha256"

type fakeEngine struct {
	listFunc    func(context.Context) ([]knarr.Image, error)
	inspectFunc func(context.Context, string) (*knarr.ImageInfo, error)
	mountFunc   func(context.Context, string, string, ...knarr.MountOption) error
	unmountFunc func(context.Context, string) error
}

func (f *fakeEngine) ObjectsDir() (string, error) {
	return testObjectsDir, nil
}

func (f *fakeEngine) List(ctx context.Context) ([]knarr.Image, error) {
	if f.listFunc == nil {
		return nil, errors.New("unexpected List")
	}
	return f.listFunc(ctx)
}

func (f *fakeEngine) Inspect(ctx context.Context, ref string) (*knarr.ImageInfo, error) {
	if f.inspectFunc == nil {
		return nil, errors.New("unexpected Inspect")
	}
	return f.inspectFunc(ctx, ref)
}

func (f *fakeEngine) Mount(ctx context.Context, ref, target string, opts ...knarr.MountOption) error {
	if f.mountFunc == nil {
		return errors.New("unexpected Mount")
	}
	return f.mountFunc(ctx, ref, target, opts...)
}

func (f *fakeEngine) Unmount(ctx context.Context, target string) error {
	if f.unmountFunc == nil {
		return errors.New("unexpected Unmount")
	}
	return f.unmountFunc(ctx, target)
}

func TestBridgeResolveIndexesLayerPrefixes(t *testing.T) {
	ctx := context.Background()
	diffIDs := []digest.Digest{
		digest.FromString("layer-1"),
		digest.FromString("layer-2"),
		digest.FromString("layer-3"),
	}
	root := digest.FromString("image-root")
	chains := chainIDs(diffIDs...)
	b := newBridge(fakeEngineForImages(testImageInfo(root, diffIDs...)))

	for i, chainID := range chains {
		entry, err := b.resolve(ctx, chainID.String())
		if err != nil {
			t.Fatalf("resolve(%s) error = %v", chainID, err)
		}
		if entry.root != root {
			t.Fatalf("resolve(%s).root = %s, want %s", chainID, entry.root, root)
		}
		wantLeaf := i == len(chains)-1
		if entry.leaf != wantLeaf {
			t.Fatalf("resolve(%s).leaf = %v, want %v", chainID, entry.leaf, wantLeaf)
		}
	}
}

func TestBridgeResolvePreservesSharedPrefixLeaf(t *testing.T) {
	ctx := context.Background()
	a := digest.FromString("shared-layer-1")
	b := digest.FromString("shared-layer-2")
	c := digest.FromString("long-layer-3")
	shortRoot := digest.FromString("short-root")
	longRoot := digest.FromString("long-root")
	shortInfo := testImageInfo(shortRoot, a, b)
	longInfo := testImageInfo(longRoot, a, b, c)
	sharedChain := chainIDs(a, b)[1].String()

	for _, tt := range []struct {
		name  string
		infos []*knarr.ImageInfo
	}{
		{name: "short first", infos: []*knarr.ImageInfo{shortInfo, longInfo}},
		{name: "long first", infos: []*knarr.ImageInfo{longInfo, shortInfo}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := newBridge(fakeEngineForImages(tt.infos...)).resolve(ctx, sharedChain)
			if err != nil {
				t.Fatalf("resolve(%s) error = %v", sharedChain, err)
			}
			if !entry.leaf {
				t.Fatalf("resolve(%s).leaf = false, want true", sharedChain)
			}
			if entry.root != shortRoot {
				t.Fatalf("resolve(%s).root = %s, want %s", sharedChain, entry.root, shortRoot)
			}
		})
	}
}

func TestBridgeResolveMissRefreshesOnce(t *testing.T) {
	ctx := context.Background()
	diffID := digest.FromString("layer")
	root := digest.FromString("root")
	chainID := chainIDs(diffID)[0].String()
	engine := fakeEngineForImages(testImageInfo(root, diffID))
	baseList := engine.listFunc
	listCalls := 0
	engine.listFunc = func(ctx context.Context) ([]knarr.Image, error) {
		listCalls++
		return baseList(ctx)
	}

	if _, err := newBridge(engine).resolve(ctx, chainID); err != nil {
		t.Fatalf("resolve(%s) error = %v", chainID, err)
	}
	if listCalls != 1 {
		t.Fatalf("List calls = %d, want 1", listCalls)
	}

	missing := digest.FromString("missing-chain").String()
	emptyEngine := fakeEngineForImages()
	emptyList := emptyEngine.listFunc
	emptyListCalls := 0
	emptyEngine.listFunc = func(ctx context.Context) ([]knarr.Image, error) {
		emptyListCalls++
		return emptyList(ctx)
	}
	if _, err := newBridge(emptyEngine).resolve(ctx, missing); !errdefs.IsNotFound(err) {
		t.Fatalf("resolve(%s) error = %v, want not found", missing, err)
	}
	if emptyListCalls != 1 {
		t.Fatalf("List calls = %d, want 1", emptyListCalls)
	}
}

func TestBridgeResolveStaleHitRefreshes(t *testing.T) {
	ctx := context.Background()
	diffID := digest.FromString("layer")
	chainID := chainIDs(diffID)[0].String()
	oldRoot := digest.FromString("old-root")

	t.Run("rematerialized", func(t *testing.T) {
		newRoot := digest.FromString("new-root")
		newInfo := testImageInfo(newRoot, diffID)
		listCalls := 0
		engine := &fakeEngine{
			listFunc: func(context.Context) ([]knarr.Image, error) {
				listCalls++
				return []knarr.Image{{Digest: newRoot.String()}}, nil
			},
			inspectFunc: func(_ context.Context, ref string) (*knarr.ImageInfo, error) {
				switch ref {
				case oldRoot.String():
					return nil, fmt.Errorf("stale image: %w", knarr.ErrNotFound)
				case newRoot.String():
					return cloneImageInfo(newInfo), nil
				default:
					return nil, fmt.Errorf("unknown image %s: %w", ref, knarr.ErrNotFound)
				}
			},
		}
		b := newBridge(engine)
		b.chains[chainID] = bridgeEntry{root: oldRoot, leaf: true}

		entry, err := b.resolve(ctx, chainID)
		if err != nil {
			t.Fatalf("resolve(%s) error = %v", chainID, err)
		}
		if entry.root != newRoot {
			t.Fatalf("resolve(%s).root = %s, want %s", chainID, entry.root, newRoot)
		}
		if listCalls != 1 {
			t.Fatalf("List calls = %d, want 1", listCalls)
		}
	})

	t.Run("gone", func(t *testing.T) {
		listCalls := 0
		engine := &fakeEngine{
			listFunc: func(context.Context) ([]knarr.Image, error) {
				listCalls++
				return nil, nil
			},
			inspectFunc: func(_ context.Context, ref string) (*knarr.ImageInfo, error) {
				if ref != oldRoot.String() {
					return nil, fmt.Errorf("unexpected Inspect(%s)", ref)
				}
				return nil, fmt.Errorf("stale image: %w", knarr.ErrNotFound)
			},
		}
		b := newBridge(engine)
		b.chains[chainID] = bridgeEntry{root: oldRoot, leaf: true}

		if _, err := b.resolve(ctx, chainID); !errdefs.IsNotFound(err) {
			t.Fatalf("resolve(%s) error = %v, want not found", chainID, err)
		}
		if listCalls != 1 {
			t.Fatalf("List calls = %d, want 1", listCalls)
		}
		if _, ok := b.chains[chainID]; ok {
			t.Fatalf("stale chain %s is still indexed", chainID)
		}
	})
}

func fakeEngineForImages(infos ...*knarr.ImageInfo) *fakeEngine {
	images := make([]knarr.Image, 0, len(infos))
	byRef := make(map[string]*knarr.ImageInfo, len(infos))
	for _, info := range infos {
		root := info.Root.String()
		images = append(images, knarr.Image{Digest: root})
		byRef[root] = cloneImageInfo(info)
	}
	return &fakeEngine{
		listFunc: func(context.Context) ([]knarr.Image, error) {
			return append([]knarr.Image(nil), images...), nil
		},
		inspectFunc: func(_ context.Context, ref string) (*knarr.ImageInfo, error) {
			info, ok := byRef[ref]
			if !ok {
				return nil, fmt.Errorf("unknown image %s: %w", ref, knarr.ErrNotFound)
			}
			return cloneImageInfo(info), nil
		},
	}
}

func testImageInfo(root digest.Digest, diffIDs ...digest.Digest) *knarr.ImageInfo {
	return &knarr.ImageInfo{
		Root:    root,
		DiffIDs: append([]digest.Digest(nil), diffIDs...),
	}
}

func cloneImageInfo(info *knarr.ImageInfo) *knarr.ImageInfo {
	clone := *info
	clone.DiffIDs = append([]digest.Digest(nil), info.DiffIDs...)
	return &clone
}

func chainIDs(diffIDs ...digest.Digest) []digest.Digest {
	return identity.ChainIDs(append([]digest.Digest(nil), diffIDs...))
}
