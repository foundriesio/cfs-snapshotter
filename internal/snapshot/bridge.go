// SPDX-License-Identifier: Apache-2.0

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/foundriesio/knarr/pkg/knarr"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
)

// engineAPI is the part of knarr.Engine consumed by the snapshotter.
type engineAPI interface {
	List(ctx context.Context) ([]knarr.Image, error)
	Inspect(ctx context.Context, ref string) (*knarr.ImageInfo, error)
	Mount(ctx context.Context, ref, target string, opts ...knarr.MountOption) error
	Unmount(ctx context.Context, target string) error
	ObjectsDir() (string, error)
}

// bridgeEntry records the image root covering a layer chain.
type bridgeEntry struct {
	root digest.Digest
	leaf bool
}

// bridge maps layer chain digests to locally materialized images.
type bridge struct {
	engine engineAPI
	mu     sync.Mutex
	chains map[string]bridgeEntry
}

func newBridge(engine engineAPI) *bridge {
	return &bridge{
		engine: engine,
		chains: make(map[string]bridgeEntry),
	}
}

func (b *bridge) resolve(ctx context.Context, chainID string) (bridgeEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if entry, ok := b.chains[chainID]; ok {
		if err := b.verify(ctx, entry); err != nil {
			if !errors.Is(err, knarr.ErrNotFound) {
				return bridgeEntry{}, fmt.Errorf("verify layer chain %s: %w", chainID, err)
			}
			delete(b.chains, chainID)
			if err := b.refreshLocked(ctx); err != nil {
				return bridgeEntry{}, fmt.Errorf("refresh local image index: %w", err)
			}
			return b.lookup(chainID)
		}
		return entry, nil
	}

	if err := b.refreshLocked(ctx); err != nil {
		return bridgeEntry{}, fmt.Errorf("refresh local image index: %w", err)
	}
	return b.lookup(chainID)
}

func (b *bridge) refreshLocked(ctx context.Context) error {
	images, err := b.engine.List(ctx)
	if err != nil {
		return fmt.Errorf("list knarr images: %w", err)
	}

	chains := make(map[string]bridgeEntry)
	for _, image := range images {
		info, err := b.engine.Inspect(ctx, image.Digest)
		if errors.Is(err, knarr.ErrNotFound) {
			log.G(ctx).WithError(err).WithField("image", image.Digest).Warn("skipping unavailable knarr image")
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect knarr image %s: %w", image.Digest, err)
		}
		if info == nil {
			return fmt.Errorf("inspect knarr image %s: nil image info", image.Digest)
		}
		for i, chainID := range identity.ChainIDs(append([]digest.Digest(nil), info.DiffIDs...)) {
			insertBridgeEntry(chains, chainID.String(), bridgeEntry{
				root: info.Root,
				leaf: i == len(info.DiffIDs)-1,
			})
		}
	}
	b.chains = chains
	return nil
}

func (b *bridge) verify(ctx context.Context, entry bridgeEntry) error {
	if _, err := b.engine.Inspect(ctx, entry.root.String()); err != nil {
		return fmt.Errorf("inspect knarr image %s: %w", entry.root, err)
	}
	return nil
}

func (b *bridge) lookup(chainID string) (bridgeEntry, error) {
	entry, ok := b.chains[chainID]
	if !ok {
		return bridgeEntry{}, fmt.Errorf("layer chain %s: %w", chainID, errdefs.ErrNotFound)
	}
	return entry, nil
}

func insertBridgeEntry(chains map[string]bridgeEntry, chainID string, entry bridgeEntry) {
	if existing, ok := chains[chainID]; ok && existing.leaf && !entry.leaf {
		return
	}
	chains[chainID] = entry
}
