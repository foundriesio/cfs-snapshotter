// SPDX-License-Identifier: Apache-2.0

// Package snapshot provides the cfs-snapshotter implementation of containerd's
// snapshots.Snapshotter interface, for use as a containerd remote (proxy)
// snapshotter.
//
// Every snapshots.Snapshotter method is currently a stub that returns
// errdefs.ErrNotImplemented.
package snapshot

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

// snapshotter is the cfs-snapshotter implementation of snapshots.Snapshotter.
type snapshotter struct {
	// root is the snapshotter's root directory.
	root string
}

// Compile-time assertion that *snapshotter satisfies the containerd interface.
var _ snapshots.Snapshotter = (*snapshotter)(nil)

// NewSnapshotter returns a snapshots.Snapshotter rooted at the given directory.
// The root directory must not be empty.
func NewSnapshotter(ctx context.Context, root string) (snapshots.Snapshotter, error) {
	if root == "" {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not be empty: %w", errdefs.ErrInvalidArgument)
	}
	return &snapshotter{root: root}, nil
}

// notImplemented wraps errdefs.ErrNotImplemented with the failing operation so
// callers and logs get context while the behavior is stubbed.
func notImplemented(op string) error {
	return fmt.Errorf("cfs snapshotter: %s: %w", op, errdefs.ErrNotImplemented)
}

func (s *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	return snapshots.Info{}, notImplemented("Stat")
}

func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	return snapshots.Info{}, notImplemented("Update")
}

func (s *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	return snapshots.Usage{}, notImplemented("Usage")
}

func (s *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	return nil, notImplemented("Mounts")
}

func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, notImplemented("Prepare")
}

func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, notImplemented("View")
}

func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return notImplemented("Commit")
}

func (s *snapshotter) Remove(ctx context.Context, key string) error {
	return notImplemented("Remove")
}

func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return notImplemented("Walk")
}

// Close releases the snapshotter's internal resources. It is safe to call on an
// already-closed snapshotter.
func (s *snapshotter) Close() error {
	return nil
}
