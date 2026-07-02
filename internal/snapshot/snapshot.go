// SPDX-License-Identifier: Apache-2.0

// Package snapshot provides the cfs-snapshotter implementation of containerd's
// snapshots.Snapshotter interface, for use as a containerd remote (proxy)
// snapshotter.
//
// Snapshot metadata and lifecycle state live in a bolt-backed metadata store
// under the root; each snapshot also owns a directory under root/snapshots.
//
// A materialized image chain is served by a single read-only mount of the
// image's EROFS metadata, executed by the snapshotter at the committed chain
// snapshot's fs directory and shared by every snapshot layered over that
// image (knarr verifies the image's fs-verity digest as part of the mount).
// Snapshots over the chain return standard overlay mount specs the consumer
// executes, with the knarr store's object directory attached as a data-only
// lower serving the file content and verity=require keeping object-level
// fs-verity enforced. The overlay is deliberately flat — one overlay over the
// EROFS metadata mount and plain directories — because a composefs mount is
// itself an overlay, so stacking another overlay on it exceeds the kernel's
// filesystem stacking-depth limit. An active snapshot's overlay is writable
// through its own upper and work directories, and committed snapshots between
// it and the chain (such as the init layer docker commits between an image
// and each container) contribute their upper directories as extra lowers. Any
// other snapshot exposes a bind of its own directory.
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
// Committed layer snapshots are always named by the cumulative chainID of
// layers 1..i, never by a single layer's digest, so the last handshake of a
// pull carries the whole image's chainID.
//
// The snapshot.ref label marks a Prepare as this handshake: it names the
// committed snapshot the unpacker actually wants. A snapshotter that can
// produce that snapshot without the caller fetching anything answers
// ErrAlreadyExists, which skips the fetch/apply/Commit round for that layer.
// This snapshotter answers that way exactly when the chain is present in the
// local knarr store (see bridge.go), so a pull of an image staged out of
// band completes with zero layer fetches (the caller still downloads the
// image metadata: index, manifest, config, etc.). A chain the store does not
// cover fails closed with a failed-precondition error naming the required
// out-of-band pull; the snapshotter never fetches content itself.
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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/foundriesio/knarr/pkg/knarr"
	"github.com/moby/sys/mountinfo"
	"github.com/opencontainers/go-digest"

	"github.com/containerd/continuity/fs"
)

// mountType and defaultMountOptions are the containerd mount type and base
// options for the bind mounts the snapshotter returns.
const mountType = "bind"

// intermediateSnapshotLabel marks a Prepare call as the unpack handshake: its
// value is the name (a cumulative chainID) under which the caller wants the
// layer chain to exist as a committed snapshot. Such snapshots are
// intermediate — they are only ever parents of other snapshots, never mounted
// themselves — and virtual: pure metadata records whose data plane (the
// shared image metadata mount at their fs directory) is established only if
// and when a descendant snapshot needs the image (see ensureChainMounted).
const intermediateSnapshotLabel = "containerd.io/snapshot.ref"

var defaultMountOptions = []string{"rbind"}

// snapshotter is the cfs-snapshotter implementation of snapshots.Snapshotter.
type snapshotter struct {
	// root is the snapshotter's root directory.
	root string
	// objectsDir is the knarr store's backing-object directory, attached to
	// every returned overlay spec as its data-only lower.
	objectsDir string
	// ms is the bolt-backed metadata store holding snapshot records.
	ms     *storage.MetaStore
	engine engineAPI
	bridge *bridge
	// mounted reports whether a path is a mount point; it is mountinfo.Mounted
	// in production and is replaced in tests.
	mounted func(string) (bool, error)
	// mountMu serializes check-and-mount and unmount of the shared chain
	// mounts, so concurrent Prepares cannot both mount the same chain.
	mountMu sync.Mutex
}

// Compile-time assertion that *snapshotter satisfies the containerd interface.
var _ snapshots.Snapshotter = (*snapshotter)(nil)

// NewSnapshotter returns a snapshots.Snapshotter rooted at root, serving
// composefs images from the knarr store at knarrRoot.
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

func newSnapshotter(ctx context.Context, root string, engine engineAPI) (*snapshotter, error) {
	if root == "" {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not be empty: %w", errdefs.ErrInvalidArgument)
	}
	if strings.ContainsAny(root, ":,") {
		return nil, fmt.Errorf("cfs snapshotter: root directory must not contain ':' or ',' (paths under it become overlay mount options): %w", errdefs.ErrInvalidArgument)
	}
	objectsDir, err := engine.ObjectsDir()
	if err != nil {
		return nil, fmt.Errorf("cfs snapshotter: resolve knarr object directory: %w", err)
	}
	if strings.ContainsAny(objectsDir, ":,") {
		return nil, fmt.Errorf("cfs snapshotter: knarr object directory must not contain ':' or ',' (it becomes an overlay mount option): %w", errdefs.ErrInvalidArgument)
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
	return &snapshotter{
		root:       root,
		objectsDir: objectsDir,
		ms:         ms,
		engine:     engine,
		bridge:     newBridge(engine),
		mounted:    mountinfo.Mounted,
	}, nil
}

// optFields renders the snapshot info fields the given opts set. An Opt
// mutates an Info, so applying the opts to a zero Info exposes exactly what
// the caller attached — in practice the labels, which carry the
// remote-snapshotter protocol (the containerd.io/snapshot.ref target and the
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

// Usage reports the disk usage of the snapshot identified by key. For an
// active snapshot it is measured live (from the overlay upper when the
// snapshot has one); for a committed snapshot the usage recorded at commit
// time is returned. After an unpack the caller queries every member of the
// image's chain in turn to price the image, so a Usage(chainID)+Stat(chainID)
// pair per layer is the normal post-pull pattern.
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
		du, err := fs.DiskUsage(ctx, s.usageDir(id))
		if err != nil {
			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
	}
	return usage, nil
}

// Mounts returns the mounts for the snapshot identified by key. Consumers
// re-fetch mount specs rather than caching what Prepare returned — a runtime
// typically calls Mounts just before starting a container — and the shared
// image metadata mount does not survive a reboot, so the specs (and the mount
// they depend on) are re-established here, not only at Prepare time.
func (s *snapshotter) Mounts(ctx context.Context, key string) (_ []mount.Mount, err error) {
	log.G(ctx).WithField("key", key).Trace("cfs snapshotter: mounts")
	var (
		sn   storage.Snapshot
		info snapshots.Info
	)
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		sn, err = storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot mount: %w", err)
		}
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return nil, err
	}
	b, backed, err := s.resolveBacking(ctx, info.Parent)
	if err != nil {
		return nil, err
	}
	// A snapshot provisioned with overlay directories is layered over an
	// image; if that image is no longer resolvable, fail rather than bind the
	// snapshot directory (which would expose upper/work as the root
	// filesystem).
	if !backed && isDir(filepath.Join(s.getSnapshotDir(sn.ID), "upper")) {
		return nil, fmt.Errorf("image backing snapshot %s is no longer in the local store: %w", key, errdefs.ErrNotFound)
	}
	return s.snapshotMounts(ctx, sn.Kind, sn.ID, backed, b)
}

// Prepare records a new active snapshot under key, descending from parent,
// and returns its mounts. Both call shapes from the package contract land
// here, and the snapshot.ref label decides the work: a handshake Prepare is
// resolved against the local image store and — when the store covers the
// chain — needs only a metadata record, created and committed virtually in
// one transaction, no directory ever provisioned, answered with
// ErrAlreadyExists instead of mounts. Everything else gets a snapshot backed
// by the image its parent chain reaches (see prepareSnapshot).
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
		// looked at. This answer, repeated per layer, is how a pull against
		// pre-fetched content completes with zero layer downloads; the caller
		// still fetches the image metadata (index, manifest, config, etc.).
		// The claim must be true before it is recorded: a chain the local
		// image store cannot cover fails closed instead — the snapshotter
		// never fetches or unpacks missing content as a fallback.
		if _, err := s.bridge.resolve(ctx, name); err != nil {
			if errdefs.IsNotFound(err) {
				return nil, fmt.Errorf("layer chain %s is not in the local image store; pull the image by digest out of band before running: %w", name, errdefs.ErrFailedPrecondition)
			}
			return nil, fmt.Errorf("resolve layer chain %s: %w", name, err)
		}
		return nil, s.createAndCommitIntermediateVirtualSnapshot(ctx, key, parent, name, base.Labels, opts)
	}

	return s.prepareSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

// View records a new read-only snapshot under key, descending from parent, and
// returns its mounts. Views are how a caller inspects committed content
// without creating a writable layer — containerd's differ and image export
// use them; the container flows never do.
func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(log.Fields{"key": key, "parent": parent}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: view")
	return s.prepareSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

// prepareSnapshot records an active or view snapshot for a container root
// filesystem and returns its mounts. A snapshot whose parent chain reaches a
// materialized image is provisioned with overlay directories and served by a
// flat overlay over the image metadata mount; any other snapshot gets a
// placeholder bind of its own
// directory.
func (s *snapshotter) prepareSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) ([]mount.Mount, error) {
	b, backed, err := s.resolveBacking(ctx, parent)
	if err != nil {
		return nil, err
	}
	sn, err := s.createSnapshot(ctx, kind, key, parent, opts, backed)
	if err != nil {
		return nil, err
	}
	mounts, err := s.snapshotMounts(ctx, sn.Kind, sn.ID, backed, b)
	if err != nil {
		// Roll the freshly created snapshot back, so retrying the same key
		// does not fail with an already-exists error.
		if rerr := s.Remove(ctx, key); rerr != nil {
			log.G(ctx).WithError(rerr).WithField("key", key).Warn("failed to roll back snapshot after mount failure")
		}
		return nil, err
	}
	return mounts, nil
}

// createAndCommitIntermediateVirtualSnapshot answers the unpack handshake:
// the snapshot the caller asks about is recorded as pure metadata — created
// and committed under the requested name, with the caller's labels and zero
// usage, in one transaction — and ErrAlreadyExists is reported so the caller
// skips fetching and applying the layer. The snapshot is virtual: nothing
// ever mounts an intermediate snapshot directly, so no directory is
// provisioned, and a concurrent handshake that loses the commit race rolls
// back whole, leaving nothing to clean up. The caller has already resolved
// the chain against the local image store, so the record's claim is backed
// by present content.
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
	}).Info("committed intermediate virtual snapshot")
	return fmt.Errorf("intermediate virtual snapshot %q: %w", name, errdefs.ErrAlreadyExists)
}

// Commit moves the active snapshot identified by key to a committed snapshot
// named name, recording its disk usage. The name encodes who is committing:
// the unpacker commits an applied layer under its cumulative chainID, and
// the docker daemon commits the init layer it populated under
// <container-id>-init so it can parent the container rootfs on it. The
// snapshotter holds no mounts for active snapshots (the consumer executes
// their overlay mounts), so there is nothing to tear down here.
func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	log.G(ctx).WithFields(log.Fields{"name": name, "key": key}).
		WithFields(optFields(opts)).Trace("cfs snapshotter: commit")
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, _, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		usage, err := fs.DiskUsage(ctx, s.usageDir(id))
		if err != nil {
			return err
		}
		if _, err := storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot: %w", err)
		}
		return nil
	})
}

// Remove abandons the snapshot identified by key, dropping its metadata
// record and the snapshot directory. Removal is driven by containerd's
// garbage collector children-first — a container's rootfs snapshot goes
// before its init layer, and image chains unwind leaf-first once no image
// references them — so the shared image metadata mount a chain snapshot
// carries is torn down inside the same transaction: a removal that would
// break live children fails before the unmount. The directory is renamed out
// of the way inside the transaction so a failure can restore it.
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
		// A failed unmount aborts the transaction with the record intact.
		if err := s.unmountDir(ctx, path); err != nil {
			return err
		}
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
// in the metadata store — including intermediate virtual snapshots — must
// surface here or it leaks.
func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	log.G(ctx).WithField("filters", filters).Trace("cfs snapshotter: walk")
	return s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, filters...)
	})
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

// backing describes how a snapshot is layered over a materialized image: the
// image root, the chain snapshot's fs directory serving as the shared image
// metadata mount point, plus the upper directories of committed snapshots
// sitting between the snapshot and the image chain (nearest parent first),
// each an extra overlay lower on top of the metadata mount.
type backing struct {
	root    digest.Digest
	chainFS string
	lowers  []string
}

// resolveBacking resolves the image a snapshot with the given parent is
// layered over. It returns the backing and true when the parent chain reaches
// a materialized image chain, walking through committed snapshots layered
// over that image (docker commits an init layer between the image and each
// container) and collecting their upper directories. It returns false when
// parent is empty or the chain does not reach a local image (a scratch
// snapshot), and an error when a parent names an intermediate layer chain
// that is not a full image and so cannot be mounted.
func (s *snapshotter) resolveBacking(ctx context.Context, parent string) (backing, bool, error) {
	var lowers []string
	for cur := parent; cur != ""; {
		entry, err := s.bridge.resolve(ctx, cur)
		if err == nil {
			if !entry.leaf {
				return backing{}, false, fmt.Errorf("layer chain %s is only a prefix of a materialized image; no image ending at this chain is in the local store, pull it by digest out of band: %w", cur, errdefs.ErrInvalidArgument)
			}
			chainID, _, err := s.snapshotInfo(ctx, cur)
			if err != nil {
				return backing{}, false, fmt.Errorf("resolve chain snapshot %s: %w", cur, err)
			}
			return backing{
				root:    entry.root,
				chainFS: filepath.Join(s.getSnapshotDir(chainID), "fs"),
				lowers:  lowers,
			}, true, nil
		}
		if !errdefs.IsNotFound(err) {
			return backing{}, false, fmt.Errorf("resolve parent chain %s: %w", cur, err)
		}
		id, info, err := s.snapshotInfo(ctx, cur)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return backing{}, false, nil
			}
			return backing{}, false, fmt.Errorf("resolve parent snapshot %s: %w", cur, err)
		}
		upper := filepath.Join(s.getSnapshotDir(id), "upper")
		if !isDir(upper) {
			// The ancestor has no overlay upper, so it was never layered over
			// an image; the whole chain is scratch.
			return backing{}, false, nil
		}
		lowers = append(lowers, upper)
		cur = info.Parent
	}
	return backing{}, false, nil
}

// createSnapshot records an active or view snapshot and provisions its
// directory. A backed active snapshot also gets the overlay upper and work
// directories its mount spec names. The directory is built under a temporary
// name and renamed into place within the transaction so a failure leaves no
// partial snapshot.
func (s *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt, backed bool) (_ storage.Snapshot, err error) {
	var (
		path, td string
		sn       storage.Snapshot
	)
	td, err = os.MkdirTemp(filepath.Join(s.root, "snapshots"), "new-")
	if err != nil {
		return storage.Snapshot{}, fmt.Errorf("failed to create temp dir: %w", err)
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
	// The snapshot directory backs a root filesystem and must be traversable by
	// the user that consumes the mount, so it is mode 0755.
	if err = os.Chmod(td, 0755); err != nil { //nolint:gosec // G302: rootfs directory must be world-traversable
		return storage.Snapshot{}, fmt.Errorf("failed to chmod %s to 0755: %w", td, err)
	}
	if backed && kind == snapshots.KindActive {
		if err = provisionOverlayDirs(td); err != nil {
			return storage.Snapshot{}, err
		}
	}

	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		sn, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if err != nil {
			return fmt.Errorf("failed to create snapshot: %w", err)
		}
		path = s.getSnapshotDir(sn.ID)
		if err = os.Rename(td, path); err != nil {
			return fmt.Errorf("failed to rename: %w", err)
		}
		td = ""
		return nil
	})
	if err != nil {
		return storage.Snapshot{}, err
	}

	log.G(ctx).WithFields(log.Fields{
		"key":    key,
		"parent": parent,
		"kind":   kind,
	}).Debug("cfs snapshotter created snapshot")

	return sn, nil
}

// snapshotMounts returns the mounts exposing a snapshot's filesystem. A backed
// snapshot returns a single flat overlay mount spec for the consumer to
// execute: the image's shared metadata mount (executed here) and any committed
// intermediate uppers are the regular lowers, and the knarr object directory
// is the data-only lower serving file content, with verity=require keeping
// object-level fs-verity enforced. The overlay is writable through the
// snapshot's upper and work directories for an active snapshot, read-only for
// a view. An unbacked snapshot is a bind of its own directory.
func (s *snapshotter) snapshotMounts(ctx context.Context, kind snapshots.Kind, id string, backed bool, b backing) ([]mount.Mount, error) {
	roFlag := "rw"
	if kind == snapshots.KindView {
		roFlag = "ro"
	}
	dir := s.getSnapshotDir(id)
	if !backed {
		return []mount.Mount{{
			Source:  dir,
			Type:    mountType,
			Options: append(defaultMountOptions, roFlag),
		}}, nil
	}
	if err := s.ensureChainMounted(ctx, b); err != nil {
		return nil, err
	}
	// The metadata mount alone carries no file data, so even a view with no
	// intermediate layers needs the overlay for its data-only lower.
	lowers := append(slices.Clone(b.lowers), b.chainFS)
	opts := []string{
		"lowerdir=" + strings.Join(lowers, ":") + "::" + s.objectsDir,
		"redirect_dir=on",
		"metacopy=on",
		"verity=require",
	}
	if kind != snapshots.KindView {
		opts = append([]string{
			"workdir=" + filepath.Join(dir, "work"),
			"upperdir=" + filepath.Join(dir, "upper"),
		}, opts...)
	}
	return []mount.Mount{{
		Source:  "overlay",
		Type:    "overlay",
		Options: opts,
	}}, nil
}

// ensureChainMounted mounts the backing image's EROFS metadata read-only at
// the chain snapshot's fs directory if it is not already mounted there (knarr
// verifies the image's fs-verity digest as part of the mount). The mount is
// shared: it serves as the bottom regular overlay lower for every snapshot
// layered over the image.
func (s *snapshotter) ensureChainMounted(ctx context.Context, b backing) error {
	s.mountMu.Lock()
	defer s.mountMu.Unlock()
	mounted, err := s.isMounted(b.chainFS)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	// The mount target backs container root filesystems, so it must be
	// world-traversable. The chain snapshot predates the first mount, so the
	// directory is created here rather than at snapshot creation.
	if err := os.MkdirAll(b.chainFS, 0755); err != nil { //nolint:gosec // G301: rootfs mount target must be world-traversable
		return fmt.Errorf("create chain mount dir %s: %w", b.chainFS, err)
	}
	if err := s.engine.Mount(ctx, b.root.String(), b.chainFS, knarr.WithMetadataOnly()); err != nil {
		return fmt.Errorf("mount image %s at %s: %w", b.root, b.chainFS, err)
	}
	return nil
}

// unmountDir tears down the shared image metadata mount at the snapshot
// directory's fs subdirectory; only chain snapshots carry one. A directory
// with no mount is left untouched.
func (s *snapshotter) unmountDir(ctx context.Context, dir string) error {
	s.mountMu.Lock()
	defer s.mountMu.Unlock()
	fsDir := filepath.Join(dir, "fs")
	mounted, err := s.isMounted(fsDir)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if err := s.engine.Unmount(ctx, fsDir); err != nil {
		return fmt.Errorf("unmount %s: %w", fsDir, err)
	}
	return nil
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

// isMounted reports whether path is a mount point, treating a missing path as
// not mounted.
func (s *snapshotter) isMounted(path string) (bool, error) {
	mounted, err := s.mounted(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("check mount %s: %w", path, err)
	}
	return mounted, nil
}

// usageDir returns the directory to measure a snapshot's usage from: the
// overlay upper for a snapshot mounted over an image, else the snapshot dir.
func (s *snapshotter) usageDir(id string) string {
	dir := s.getSnapshotDir(id)
	if upper := filepath.Join(dir, "upper"); isDir(upper) {
		return upper
	}
	return dir
}

// getSnapshotDir returns the directory backing the snapshot with the given id.
func (s *snapshotter) getSnapshotDir(id string) string {
	return filepath.Join(s.root, "snapshots", id)
}

// provisionOverlayDirs creates the overlay upper and work directories an
// active snapshot's mount spec names inside its snapshot directory.
func provisionOverlayDirs(dir string) error {
	for _, name := range []string{"upper", "work"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			return fmt.Errorf("create %s dir: %w", name, err)
		}
	}
	return nil
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
