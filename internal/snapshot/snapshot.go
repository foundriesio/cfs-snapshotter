// SPDX-License-Identifier: Apache-2.0

// Package snapshot provides the cfs-snapshotter implementation of containerd's
// snapshots.Snapshotter interface, for use as a containerd remote (proxy)
// snapshotter.
//
// Snapshot metadata and lifecycle state live in a bolt-backed metadata store
// under the root; each directory-backed snapshot also owns a directory under
// root/snapshots. The filesystem a snapshot mount exposes is a placeholder
// bind of that directory: content-addressed composefs materialization
// replaces mounts and the create-time directory step in a later change. Until
// then the implementation is a remote-snapshotter stub: every unpack
// handshake is answered as if the content were already present locally (see
// the calling contract below).
//
// # Calling contract
//
// Two callers drive this interface, distinguishable by request shape.
//
// Image unpack (containerd's unpacker, on behalf of an image pull) walks the
// layers bottom-up. For each layer i it asks whether the cumulative chain
// 1..i already exists as a committed snapshot and, if not, prepares and
// populates it:
//
//	Stat(chainID(1..i))                              -> ErrNotFound
//	Prepare(key="<ns>/extract-<rnd> <chainID(1..i)>",
//	        parent=chainID(1..i-1),
//	        labels["containerd.io/snapshot.ref"]=chainID(1..i))
//	  ... the caller fetches and applies the layer tar into the mounts ...
//	Commit(name=chainID(1..i), key=<the extract key>)
//	Stat(chainID(1..i))                              -> confirmation
//
// The snapshot.ref label marks a Prepare as this handshake: it names the
// committed snapshot the unpacker actually wants. A snapshotter that can
// produce that snapshot without the caller fetching anything answers
// ErrAlreadyExists, which skips the fetch/apply/Commit round for that layer.
// This implementation always answers that way — it is a stub of the remote
// path: the committed records carry no image content, so the pull completes
// with zero layer fetches and nothing stands behind the chain. Materializing
// the real image content behind the handshake is what replaces this stub.
// Committed layer snapshots are always named by the cumulative chainID of
// layers 1..i, never by a single layer's digest, so the last handshake of a
// pull carries the whole image's chainID.
//
// Container creation (the docker daemon, via containerd) prepares two
// snapshots on top of the image's full chain:
//
//	Prepare(key=<id>-init, parent=chainID(1..n))     -> mounts
//	  ... docker seeds runtime files: /etc/hostname, /etc/hosts, ... ...
//	Commit(name=<id>-init, key=<id>-init)
//	Prepare(key=<id>, parent=<id>-init)              -> the rootfs mounts
//
// The committed init layer keeps those runtime files out of both the image
// and the container's own diff. Neither call carries snapshot.ref: the
// absence of that label is what separates a snapshot somebody will mount and
// write to from unpack bookkeeping.
//
// Around both flows, Stat, Usage, and Walk serve size accounting,
// parent-chain resolution, and the metadata garbage collector — Stat in
// particular fires repeatedly per committed name — and Remove tears
// snapshots down children-first.
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

	"github.com/containerd/continuity/fs"
)

// intermediateSnapshotLabel marks a Prepare call as the unpack handshake: its
// value is the name (a cumulative chainID) under which the caller wants the
// layer chain to exist as a committed snapshot. Such snapshots are
// intermediate — they are only ever parents of other snapshots, never mounted
// themselves — and virtual: pure metadata records with no directory behind
// them.
const intermediateSnapshotLabel = "containerd.io/snapshot.ref"

// mountType and defaultMountOptions describe the placeholder mount returned for
// a snapshot until composefs materialization lands.
const mountType = "bind"

var defaultMountOptions = []string{"rbind"}

// snapshotter is the cfs-snapshotter implementation of snapshots.Snapshotter.
type snapshotter struct {
	// root is the snapshotter's root directory.
	root string
	// ms is the bolt-backed metadata store holding snapshot records.
	ms *storage.MetaStore
}

// Compile-time assertion that *snapshotter satisfies the containerd interface.
var _ snapshots.Snapshotter = (*snapshotter)(nil)

// NewSnapshotter returns a snapshots.Snapshotter rooted at the given directory.
// The root directory must not be empty. It is created if it does not exist,
// along with the metadata store and the snapshots directory beneath it.
func NewSnapshotter(ctx context.Context, root string) (snapshots.Snapshotter, error) {
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
	return &snapshotter{root: root, ms: ms}, nil
}

// optFields renders the snapshot info fields the given opts set. An Opt
// mutates an Info, so applying the opts to a zero Info exposes exactly what
// the caller attached — in practice the labels, which carry the
// remote-snapshotter protocol (the containerd.io/snapshot.ref name and the
// containerd.io/snapshot/* chain labels) and are otherwise invisible until
// they land in the metadata store.
func optFields(opts []snapshots.Opt) log.Fields {
	var info snapshots.Info
	for _, opt := range opts {
		if err := opt(&info); err != nil {
			return log.Fields{"optserror": err.Error()}
		}
	}
	fields := log.Fields{}
	if info.Name != "" {
		fields["optname"] = info.Name
	}
	if info.Parent != "" {
		fields["optparent"] = info.Parent
	}
	if len(info.Labels) > 0 {
		fields["labels"] = info.Labels
	}
	return fields
}

// Stat returns the info for an active or committed snapshot by key. It is the
// hottest read on the interface: the unpacker probes each chainID before its
// Prepare handshake and confirms it after, usage accounting re-resolves every
// chain member, and any parent-chain walk re-Stats each ancestor — so one
// committed name is Stat'd many times over its life. It must stay a cheap
// metadata lookup.
func (s *snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: stat")
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
// fieldpaths, when given, restrict which fields are written. In practice the
// caller is containerd's garbage collector flipping retention labels
// (containerd.io/gc.*) on committed snapshots.
func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (_ snapshots.Info, err error) {
	log.G(ctx).WithFields(log.Fields{
		"name":       info.Name,
		"kind":       info.Kind,
		"labels":     info.Labels,
		"fieldpaths": fieldpaths,
	}).Trace("cfs snapshotter: update")
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
// the usage recorded at commit time is returned. After an unpack the caller
// queries every member of the image's chain in turn to price the image, so a
// Usage(chainID)+Stat(chainID) pair per layer is the normal post-pull pattern.
func (s *snapshotter) Usage(ctx context.Context, key string) (usage snapshots.Usage, err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: usage")
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

// Mounts returns the mounts for the snapshot identified by key. Consumers
// re-fetch mount specs rather than caching what Prepare returned — a runtime
// typically calls Mounts just before starting a container — so the specs must
// stay reproducible for the lifetime of the snapshot.
func (s *snapshotter) Mounts(ctx context.Context, key string) (_ []mount.Mount, err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: mounts")
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
// returns its mounts. Both call shapes from the package contract land here:
// the unpacker's per-layer handshake (snapshot.ref label present, extract key,
// mounts used only to apply one layer tar) and real writable snapshots — the
// docker init layer and the container rootfs (no snapshot.ref, mounts actually
// executed). The snapshot.ref label decides the shape of the work: a
// handshake asks for an intermediate snapshot, which needs only a metadata
// record — created and committed virtually in one transaction, no snapshot
// directory ever provisioned — and is answered with ErrAlreadyExists instead
// of mounts. Everything else gets a directory-backed active snapshot served
// from the placeholder bind.
func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(log.Fields{"key": key, "parent": parent}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: prepare")
	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return nil, fmt.Errorf("prepare options: %w", err)
		}
	}

	if name, ok := base.Labels[intermediateSnapshotLabel]; ok {
		// This Prepare is one step of the image-unpack handshake: while a
		// pull walks the layers bottom-up, the unpacker prepares a throwaway
		// snapshot per layer, labeled with the name (the cumulative chainID)
		// it wants that layer chain to exist under, before fetching the
		// layer. Committing the intermediate virtual snapshot under that name
		// and answering ErrAlreadyExists tells the caller the chain is
		// already present locally: it skips the layer's fetch, apply, and
		// commit, and moves on to the next layer — the nil mounts are never
		// looked at. This answer, repeated per layer, is how a pull completes
		// with zero layer downloads against pre-fetched content.
		return nil, s.createAndCommitIntermediateVirtualSnapshot(ctx, key, parent, name, base.Labels, opts)
	}

	return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

// createAndCommitIntermediateVirtualSnapshot answers the unpack handshake:
// the intermediate snapshot the caller asks about is recorded as pure
// metadata — the active snapshot and its commit under the requested name,
// with the caller's labels and zero usage, in a single transaction — and
// ErrAlreadyExists is reported so the caller skips fetching and applying the
// layer. The snapshot is virtual: nothing ever mounts an intermediate
// snapshot, so no directory is provisioned for it, and a concurrent handshake
// that loses the commit race is rolled back whole, leaving no record to clean
// up. In this stub the record only claims the layer chain exists — no content
// stands behind it; the replacement of the stub verifies the content is
// really present locally before answering.
func (s *snapshotter) createAndCommitIntermediateVirtualSnapshot(ctx context.Context, key, parent, name string, labels map[string]string, opts []snapshots.Opt) error {
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		if _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...); err != nil {
			return fmt.Errorf("create intermediate snapshot %q: %w", name, err)
		}
		if _, err := storage.CommitActive(ctx, key, name, snapshots.Usage{}, append(opts, snapshots.WithLabels(labels))...); err != nil {
			return fmt.Errorf("commit intermediate snapshot %q: %w", name, err)
		}
		return nil
	})
	if err != nil && !errdefs.IsAlreadyExists(err) {
		return err
	}
	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"name":   name,
		"parent": parent,
	}).Info("stub-committed intermediate virtual snapshot")
	return fmt.Errorf("intermediate virtual snapshot %q: %w", name, errdefs.ErrAlreadyExists)
}

// View records a new read-only snapshot under key, descending from parent, and
// returns its mounts. Views are how a caller inspects committed content
// without creating a writable layer — containerd's differ and image export
// use them; the container flows never do.
func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(log.Fields{"key": key, "parent": parent}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: view")
	return s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

// Commit moves the active snapshot identified by key to a committed snapshot
// named name, recording its disk usage. The name encodes who is committing:
// the unpacker commits an applied layer under its cumulative chainID, and the
// docker daemon commits the seeded init layer under <container-id>-init so it
// can parent the container rootfs on it. A committed snapshot is immutable
// and exists to be a parent.
func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	log.G(ctx).WithFields(log.Fields{"name": name, "key": key}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: commit")
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
// the transaction so a failure can restore it. Removal is driven by
// containerd's garbage collector children-first — a container's rootfs
// snapshot goes before its init layer, and image chains unwind leaf-first
// once no image references them.
func (s *snapshotter) Remove(ctx context.Context, key string) (err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: remove")
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

// Walk invokes fn for each committed snapshot matching the given filters. The
// garbage collector uses it to enumerate what exists, so everything recorded
// in the metadata store — including bookkeeping snapshots — must surface here
// or it leaks.
func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	log.G(ctx).WithField("filters", filters).Trace("cfs snapshotter: walk")
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
	log.L.Trace("cfs snapshotter: close")
	if s.ms == nil {
		return nil
	}
	return s.ms.Close()
}
