// SPDX-License-Identifier: Apache-2.0

// Package snapshot provides the cfs-snapshotter implementation of containerd's
// snapshots.Snapshotter interface, for use as a containerd remote (proxy)
// snapshotter.
//
// Snapshot metadata and lifecycle state live in a bolt-backed metadata store
// under the root; each snapshot also owns a directory under root/snapshots. The
// filesystem a snapshot mount exposes is a placeholder bind of that directory:
// content-addressed composefs materialization replaces mounts and the
// create-time directory step in a later change.
package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/foundriesio/knarr/pkg/knarr"

	"github.com/containerd/continuity/fs"
)

// mountType and defaultMountOptions describe the placeholder mount returned for
// a snapshot until composefs materialization lands.
const mountType = "bind"

// targetSnapshotLabel marks a Prepare call as a request for a committed
// snapshot with the given name; the label value is the snapshot name to
// commit under.
const targetSnapshotLabel = "containerd.io/snapshot.ref"

var defaultMountOptions = []string{"rbind"}

// snapshotter is the cfs-snapshotter implementation of snapshots.Snapshotter.
type snapshotter struct {
	// root is the snapshotter's root directory.
	root string
	// ms is the bolt-backed metadata store holding snapshot records.
	ms     *storage.MetaStore
	engine engineAPI
	bridge *bridge
}

// Compile-time assertion that *snapshotter satisfies the containerd interface.
var _ snapshots.Snapshotter = (*snapshotter)(nil)

// NewSnapshotter returns a snapshots.Snapshotter rooted at the given directory.
func NewSnapshotter(ctx context.Context, root, knarrRoot string) (snapshots.Snapshotter, error) {
	if root == "" {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not be empty: %w", errdefs.ErrInvalidArgument)
	}
	engine, err := knarr.New(knarr.Config{Root: knarrRoot})
	if err != nil {
		return nil, fmt.Errorf("create knarr engine: %w", err)
	}
	return newSnapshotter(ctx, root, engine)
}

func newSnapshotter(ctx context.Context, root string, engine engineAPI) (snapshots.Snapshotter, error) {
	if root == "" {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not be empty: %w", errdefs.ErrInvalidArgument)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("cfs snapshotter: create root: %w", err)
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, fmt.Errorf("cfs snapshotter: open metadata store: %w", err)
	}
	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("cfs snapshotter: create snapshots dir: %w", err)
	}
	return &snapshotter{root: root, ms: ms, engine: engine, bridge: newBridge(engine)}, nil
}

// Stat returns the info for an active or committed snapshot by key. It is used
// for parent resolution, existence checks, and to discern the snapshot kind.
func (s *snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}
	return info, nil
}

// Update modifies the labels and timestamps of an existing snapshot's info. The
// fieldpaths, when given, restrict which fields are written.
func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (_ snapshots.Info, err error) {
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}
	return info, nil
}

// Usage reports the disk usage of the snapshot identified by key. For an active
// snapshot it is measured from the snapshot directory; for a committed snapshot
// the usage recorded at commit time is returned.
func (s *snapshotter) Usage(ctx context.Context, key string) (usage snapshots.Usage, err error) {
	var (
		id   string
		info snapshots.Info
	)
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, usage, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return snapshots.Usage{}, err
	}
	if info.Kind == snapshots.KindActive {
		du, err := fs.DiskUsage(ctx, s.getSnapshotDir(id))
		if err != nil {
			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
	}
	return usage, nil
}

// Mounts returns the mounts for the snapshot identified by key. It can be used
// to recover the mounts produced by a prior Prepare or View.
func (s *snapshotter) Mounts(ctx context.Context, key string) (_ []mount.Mount, err error) {
	var sn storage.Snapshot
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		sn, err = storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot mount: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.mounts(sn), nil
}

// Prepare records a new active snapshot under key, descending from parent, and
// returns its mounts.
func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return nil, fmt.Errorf("apply snapshot option: %w", err)
		}
	}
	target, ok := base.Labels[targetSnapshotLabel]
	if !ok {
		return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
	}
	if _, err := s.bridge.resolve(ctx, target); err != nil {
		if errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("layer chain %s is not in the local image store; pull the image by digest out of band before running: %w", target, errdefs.ErrFailedPrecondition)
		}
		return nil, fmt.Errorf("resolve layer chain %s: %w", target, err)
	}
	return nil, s.prepareTarget(ctx, key, parent, target, base.Labels, opts)
}

// View records a new read-only snapshot under key, descending from parent, and
// returns its mounts.
func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

// prepareTarget handles a Prepare carrying targetSnapshotLabel: it creates
// the active snapshot, immediately commits it under the target name with the
// caller's labels and parent, and reports ErrAlreadyExists. The committed
// snapshot has no content; it only records that the target is present so the
// caller can adopt it by walking for the label and parent.
func (s *snapshotter) prepareTarget(ctx context.Context, key, parent, target string, labels map[string]string, opts []snapshots.Opt) error {
	if _, err := s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts); err != nil {
		return fmt.Errorf("create target snapshot %q: %w", target, err)
	}
	err := s.Commit(ctx, target, key, append(opts, snapshots.WithLabels(labels))...)
	if err != nil && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("commit target snapshot %q: %w", target, err)
	}
	if errdefs.IsAlreadyExists(err) {
		// The target was committed by an earlier call; drop the extra
		// active snapshot so no orphan record is left behind.
		if rerr := s.Remove(ctx, key); rerr != nil {
			log.G(ctx).WithError(rerr).WithField("key", key).Warn("failed to remove superseded active snapshot")
		}
	}
	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"target": target,
		"parent": parent,
	}).Info("committed remote snapshot target")
	return fmt.Errorf("target snapshot %q: %w", target, errdefs.ErrAlreadyExists)
}

// Commit moves the active snapshot identified by key to a committed snapshot
// named name, recording its disk usage.
func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, _, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		usage, err := fs.DiskUsage(ctx, s.getSnapshotDir(id))
		if err != nil {
			return err
		}
		if _, err := storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot: %w", err)
		}
		return nil
	})
}

// Remove abandons the snapshot identified by key, dropping its metadata record
// and the snapshot directory. The directory is renamed out of the way inside
// the transaction so a failure can restore it.
func (s *snapshotter) Remove(ctx context.Context, key string) (err error) {
	var (
		renamed, path string
		restore       bool
	)
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, _, err := storage.Remove(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to remove: %w", err)
		}
		path = s.getSnapshotDir(id)
		renamed = filepath.Join(s.root, "snapshots", "rm-"+id)
		if err = os.Rename(path, renamed); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("failed to rename: %w", err)
			}
			renamed = ""
		}
		restore = true
		return nil
	})
	if err != nil {
		if renamed != "" && restore {
			if err1 := os.Rename(renamed, path); err1 != nil {
				// May cause inconsistent data on disk.
				log.G(ctx).WithError(err1).WithField("path", renamed).Error("failed to rename after failed remove")
			}
		}
		return err
	}
	if renamed != "" {
		if err := os.RemoveAll(renamed); err != nil {
			// Must be cleaned up; any "rm-*" left behind can be removed when no
			// transactions are active.
			log.G(ctx).WithError(err).WithField("path", renamed).Warn("failed to remove snapshot directory")
		}
	}
	return nil
}

// Walk invokes fn for each committed snapshot matching the given filters.
func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, filters...)
	})
}

// createSnapshot records an active or view snapshot and provisions its
// directory. The directory is built under a temporary name and renamed into
// place within the transaction so a failure leaves no partial snapshot.
func (s *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	var (
		path, td string
		sn       storage.Snapshot
	)
	td, err = os.MkdirTemp(filepath.Join(s.root, "snapshots"), "new-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	// The snapshot directory backs a root filesystem and must be traversable by
	// the user that consumes the mount, so it is mode 0755.
	if err := os.Chmod(td, 0755); err != nil { //nolint:gosec // G302: rootfs directory must be world-traversable
		return nil, fmt.Errorf("failed to chmod %s to 0755: %w", td, err)
	}
	defer func() {
		if err != nil {
			if td != "" {
				if err1 := os.RemoveAll(td); err1 != nil {
					err = fmt.Errorf("remove failed: %v: %w", err1, err)
				}
			}
			if path != "" {
				if err1 := os.RemoveAll(path); err1 != nil {
					err = fmt.Errorf("failed to remove path: %v: %w", err1, err)
				}
			}
		}
	}()

	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		sn, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if err != nil {
			return fmt.Errorf("failed to create snapshot: %w", err)
		}
		// The parent chain is recorded in metadata (sn.ParentIDs); composefs
		// will compose it at mount time. No parent content is copied here.
		path = s.getSnapshotDir(sn.ID)
		if err = os.Rename(td, path); err != nil {
			return fmt.Errorf("failed to rename: %w", err)
		}
		td = ""
		return nil
	})
	if err != nil {
		return nil, err
	}

	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"parent": parent,
		"kind":   kind,
	}).Debug("cfs snapshotter created snapshot")

	return s.mounts(sn), nil
}

// getSnapshotDir returns the directory backing the snapshot with the given id.
func (s *snapshotter) getSnapshotDir(id string) string {
	return filepath.Join(s.root, "snapshots", id)
}

// mounts returns the mounts exposing the snapshot's filesystem. This is the
// single seam where content-addressed composefs mounts replace the placeholder:
// today it binds the snapshot's own directory; composefs will compose the
// snapshot's parent chain into one mount.
func (s *snapshotter) mounts(sn storage.Snapshot) []mount.Mount {
	roFlag := "rw"
	if sn.Kind == snapshots.KindView {
		roFlag = "ro"
	}
	return []mount.Mount{
		{
			Source:  s.getSnapshotDir(sn.ID),
			Type:    mountType,
			Options: append(defaultMountOptions, roFlag),
		},
	}
}

// Close releases the snapshotter's metadata store. It is safe to call on an
// already-closed snapshotter.
func (s *snapshotter) Close() error {
	if s.ms == nil {
		return nil
	}
	return s.ms.Close()
}
